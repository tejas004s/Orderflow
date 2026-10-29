// Package marketdata implements the Geometric Brownian Motion (GBM) price
// simulator and 1-minute OHLCV candle aggregator (SDD sections 3.1, 10).
package marketdata

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/tejas004s/orderflow/internal/events"
	"github.com/tejas004s/orderflow/internal/kafka"
	"github.com/tejas004s/orderflow/internal/outbox"
)

type symState struct {
	symbol   string
	tickSize int64
	refPrice int64
	price    float64
	candle   *events.Candle
}

// Service runs the tick generator and candle aggregator.
type Service struct {
	Pool         *pgxpool.Pool
	Pub          outbox.Publisher
	Log          *slog.Logger
	TickInterval time.Duration
	Drift        float64 // annualized drift mu
	Volatility   float64 // annualized volatility sigma

	mu   sync.Mutex
	syms map[string]*symState
	rng  *rand.Rand
}

// NewService constructs a marketdata Service.
func NewService(pool *pgxpool.Pool, pub outbox.Publisher, log *slog.Logger) *Service {
	return &Service{
		Pool:         pool,
		Pub:          pub,
		Log:          log,
		TickInterval: 750 * time.Millisecond,
		Drift:        0.05,
		Volatility:   0.35,
		syms:         map[string]*symState{},
		rng:          rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 0x9e3779b97f4a7c15)),
	}
}

// Init loads active instruments from Postgres and seeds 60 minutes of
// historical 1m candles if the candles table is empty so charts look alive
// immediately on startup.
func (s *Service) Init(ctx context.Context) error {
	rows, err := s.Pool.Query(ctx, `
		SELECT symbol, tick_size, ref_price FROM instruments WHERE status = 'ACTIVE' ORDER BY symbol
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	s.mu.Lock()
	defer s.mu.Unlock()
	for rows.Next() {
		var sym string
		var tick, ref int64
		if err := rows.Scan(&sym, &tick, &ref); err != nil {
			return err
		}
		s.syms[sym] = &symState{
			symbol:   sym,
			tickSize: max(tick, 1),
			refPrice: ref,
			price:    float64(ref),
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	nowBucket := time.Now().UTC().Truncate(time.Minute)
	for _, st := range s.syms {
		var count int
		_ = s.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM candles WHERE symbol = $1 AND interval = '1m'`, st.symbol).Scan(&count)
		if count >= 30 {
			// Load most recent close price to continue seamlessly.
			var lastClose int64
			if err := s.Pool.QueryRow(ctx, `
				SELECT close FROM candles WHERE symbol = $1 AND interval = '1m' ORDER BY bucket_start DESC LIMIT 1
			`, st.symbol).Scan(&lastClose); err == nil && lastClose > 0 {
				st.price = float64(lastClose)
			}
			continue
		}
		// Seed 60 historical 1-minute candles leading up to nowBucket.
		px := float64(st.refPrice) * 0.98
		for i := 60; i >= 1; i-- {
			bStart := nowBucket.Add(-time.Duration(i) * time.Minute)
			open := roundTick(px, st.tickSize)
			high, low := open, open
			var vol int64
			for step := 0; step < 8; step++ {
				px = stepGBM(s.rng, px, st.refPrice, s.Drift, s.Volatility, 1.0/(252*390*8))
				p := roundTick(px, st.tickSize)
				high = max(high, p)
				low = min(low, p)
				vol += int64(s.rng.IntN(40) + 5)
			}
			closePx := roundTick(px, st.tickSize)
			_ = s.upsertCandle(ctx, events.Candle{
				Symbol:      st.symbol,
				Interval:    "1m",
				BucketStart: bStart.Unix(),
				Open:        open,
				High:        high,
				Low:         low,
				Close:       closePx,
				Volume:      vol,
			})
		}
		st.price = px
	}
	return nil
}

// StepOnce advances every symbol by one GBM step, updates its current 1m
// candle, persists to Postgres, and publishes tick + candle events.
func (s *Service) StepOnce(ctx context.Context, now time.Time) error {
	s.mu.Lock()
	var msgs []outbox.Message
	var toUpsert []events.Candle
	dt := 1.0 / (252.0 * 6.5 * 3600.0) // ~1 second of trading time

	for _, st := range s.syms {
		st.price = stepGBM(s.rng, st.price, st.refPrice, s.Drift, s.Volatility, dt)
		px := roundTick(st.price, st.tickSize)
		tk := events.MarketTick{
			Symbol:    st.symbol,
			Price:     px,
			Volume:    0,
			Timestamp: now.UnixNano(),
		}
		cd := s.applyPriceSampleLocked(st, px, 0, now)
		toUpsert = append(toUpsert, cd)

		if rawTick, err := events.Wrap(uuid.New(), events.TypeMarketTick, "marketdata-service", "", tk.Timestamp, tk); err == nil {
			msgs = append(msgs, outbox.Message{
				Topic:   kafka.TopicTicks,
				Key:     []byte(st.symbol),
				Payload: rawTick,
				Headers: map[string]string{"event_type": events.TypeMarketTick},
			})
		}
		if rawCandle, err := events.Wrap(uuid.New(), events.TypeCandle1m, "marketdata-service", "", now.UnixNano(), cd); err == nil {
			msgs = append(msgs, outbox.Message{
				Topic:   kafka.TopicCandles1m,
				Key:     []byte(st.symbol),
				Payload: rawCandle,
				Headers: map[string]string{"event_type": events.TypeCandle1m},
			})
		}
	}
	s.mu.Unlock()

	for _, cd := range toUpsert {
		if err := s.upsertCandle(ctx, cd); err != nil {
			return err
		}
	}
	if s.Pub != nil && len(msgs) > 0 {
		if err := s.Pub.Publish(ctx, msgs); err != nil {
			return fmt.Errorf("publish marketdata: %w", err)
		}
	}
	return nil
}

// OnTrade incorporates an executed trade into the symbol's GBM anchor price and
// 1-minute OHLCV candle (SDD section 10).
func (s *Service) OnTrade(ctx context.Context, tr events.TradeExecuted) error {
	ts := time.Unix(0, tr.ExecutedAt).UTC()
	if tr.ExecutedAt == 0 {
		ts = time.Now().UTC()
	}

	s.mu.Lock()
	st, ok := s.syms[tr.Symbol]
	if !ok {
		st = &symState{symbol: tr.Symbol, tickSize: 1, refPrice: tr.Price, price: float64(tr.Price)}
		s.syms[tr.Symbol] = st
	}
	st.price = float64(tr.Price)
	cd := s.applyPriceSampleLocked(st, tr.Price, tr.Qty, ts)
	s.mu.Unlock()

	if err := s.upsertCandle(ctx, cd); err != nil {
		return err
	}
	if s.Pub != nil {
		if raw, err := events.Wrap(uuid.New(), events.TypeCandle1m, "marketdata-service", "", ts.UnixNano(), cd); err == nil {
			_ = s.Pub.Publish(ctx, []outbox.Message{{
				Topic:   kafka.TopicCandles1m,
				Key:     []byte(cd.Symbol),
				Payload: raw,
				Headers: map[string]string{"event_type": events.TypeCandle1m},
			}})
		}
	}
	return nil
}

func (s *Service) applyPriceSampleLocked(st *symState, price, vol int64, ts time.Time) events.Candle {
	bucket := ts.UTC().Truncate(time.Minute).Unix()
	if st.candle == nil || st.candle.BucketStart != bucket {
		st.candle = &events.Candle{
			Symbol:      st.symbol,
			Interval:    "1m",
			BucketStart: bucket,
			Open:        price,
			High:        price,
			Low:         price,
			Close:       price,
			Volume:      vol,
		}
		return *st.candle
	}
	st.candle.High = max(st.candle.High, price)
	st.candle.Low = min(st.candle.Low, price)
	st.candle.Close = price
	st.candle.Volume += vol
	return *st.candle
}

func (s *Service) upsertCandle(ctx context.Context, c events.Candle) error {
	if s.Pool == nil {
		return nil
	}
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO candles (symbol, interval, bucket_start, open, high, low, close, volume)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (symbol, interval, bucket_start) DO UPDATE
		SET high   = GREATEST(candles.high, EXCLUDED.high),
		    low    = LEAST(candles.low, EXCLUDED.low),
		    close  = EXCLUDED.close,
		    volume = GREATEST(candles.volume, EXCLUDED.volume)
	`, c.Symbol, c.Interval, time.Unix(c.BucketStart, 0).UTC(), c.Open, c.High, c.Low, c.Close, c.Volume)
	return err
}

// RunSimulator ticks every TickInterval until ctx is cancelled.
func (s *Service) RunSimulator(ctx context.Context) error {
	ticker := time.NewTicker(s.TickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case now := <-ticker.C:
			if err := s.StepOnce(ctx, now.UTC()); err != nil && ctx.Err() == nil {
				s.Log.Warn("marketdata step failed", "err", err)
			}
		}
	}
}

// RunTradeConsumer consumes trades.executed and updates 1m candles.
func (s *Service) RunTradeConsumer(ctx context.Context, cl *kgo.Client) error {
	for {
		fetches := cl.PollFetches(ctx)
		if fetches.IsClientClosed() || ctx.Err() != nil {
			return ctx.Err()
		}
		fetches.EachRecord(func(rec *kgo.Record) {
			var tr events.TradeExecuted
			if _, err := events.Unwrap(rec.Value, &tr); err == nil {
				_ = s.OnTrade(ctx, tr)
			}
			_ = cl.CommitRecords(ctx, rec)
		})
	}
}

func stepGBM(rng *rand.Rand, price float64, ref int64, mu, sigma, dt float64) float64 {
	z := rng.NormFloat64()
	next := price * math.Exp((mu-0.5*sigma*sigma)*dt+sigma*math.Sqrt(dt)*z)
	lo := float64(ref) * 0.5
	hi := float64(ref) * 2.0
	return math.Max(lo, math.Min(hi, next))
}

func roundTick(px float64, tick int64) int64 {
	if tick <= 0 {
		tick = 1
	}
	v := int64(math.Round(px))
	if rem := v % tick; rem != 0 {
		v -= rem
	}
	if v < tick {
		v = tick
	}
	return v
}
