// Package matching wires the pure matching engine (internal/engine) to Kafka
// partitions and PostgreSQL snapshots (SDD sections 4.2, 5.4).
package matching

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/tejas004s/orderflow/internal/engine"
	"github.com/tejas004s/orderflow/internal/events"
	"github.com/tejas004s/orderflow/internal/httpx"
	"github.com/tejas004s/orderflow/internal/kafka"
	"github.com/tejas004s/orderflow/internal/obs"
	"github.com/tejas004s/orderflow/internal/outbox"
)

// SnapshotStore abstracts reading and writing partition snapshots.
type SnapshotStore interface {
	Load(ctx context.Context, partition int32) (nextOffset int64, blob []byte, ok bool, err error)
	Save(ctx context.Context, partition int32, nextOffset int64, blob []byte) error
}

// PGSnapshotStore stores engine snapshots in the engine_snapshots table.
type PGSnapshotStore struct {
	Pool *pgxpool.Pool
}

// Load retrieves the latest snapshot for partition, if any.
func (s PGSnapshotStore) Load(ctx context.Context, partition int32) (int64, []byte, bool, error) {
	if s.Pool == nil {
		return 0, nil, false, nil
	}
	var nextOffset int64
	var blob []byte
	err := s.Pool.QueryRow(ctx, `
		SELECT next_offset, snapshot FROM engine_snapshots WHERE partition = $1
	`, partition).Scan(&nextOffset, &blob)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil, false, nil
	}
	if err != nil {
		return 0, nil, false, err
	}
	return nextOffset, blob, true, nil
}

// Save upserts the snapshot for partition.
func (s PGSnapshotStore) Save(ctx context.Context, partition int32, nextOffset int64, blob []byte) error {
	if s.Pool == nil {
		return nil
	}
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO engine_snapshots (partition, next_offset, snapshot, taken_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (partition) DO UPDATE
		SET next_offset = EXCLUDED.next_offset,
		    snapshot    = EXCLUDED.snapshot,
		    taken_at    = EXCLUDED.taken_at
	`, partition, nextOffset, blob)
	return err
}

// DepthSnapshot is a cached L2 depth snapshot of one symbol.
type DepthSnapshot struct {
	Symbol string         `json:"symbol"`
	Seq    uint64         `json:"seq"`
	Bids   []engine.Level `json:"bids"`
	Asks   []engine.Level `json:"asks"`
}

// Processor owns the partition engines and translates commands into outbound
// Kafka messages on orders.events and trades.executed.
type Processor struct {
	Pub           outbox.Publisher
	Snapshots     SnapshotStore
	Log           *slog.Logger
	SnapshotEvery int64 // commands between snapshots (default 1000)

	mu         sync.RWMutex
	partitions map[int32]*partState
	books      map[string]DepthSnapshot
}

type partState struct {
	eng        *engine.Engine
	cmdsSince  int64
	nextOffset int64
}

// NewProcessor constructs a Processor.
func NewProcessor(pub outbox.Publisher, snaps SnapshotStore, log *slog.Logger) *Processor {
	return &Processor{
		Pub:           pub,
		Snapshots:     snaps,
		Log:           log,
		SnapshotEvery: 1000,
		partitions:    map[int32]*partState{},
		books:         map[string]DepthSnapshot{},
	}
}

// InitPartition restores a partition's engine from SnapshotStore if available
// and returns the next offset to consume from.
func (p *Processor) InitPartition(ctx context.Context, part int32) (int64, error) {
	eng := engine.New()
	var nextOffset int64
	if p.Snapshots != nil {
		off, blob, ok, err := p.Snapshots.Load(ctx, part)
		if err != nil {
			return 0, err
		}
		if ok {
			restored, err := engine.Restore(blob)
			if err != nil {
				return 0, fmt.Errorf("restore partition %d: %w", part, err)
			}
			eng = restored
			nextOffset = off
		}
	}
	p.mu.Lock()
	p.partitions[part] = &partState{eng: eng, nextOffset: nextOffset}
	p.mu.Unlock()
	return nextOffset, nil
}

// HandleRaw processes one raw Kafka record from orders.commands on partition
// part at offset. Poison messages that fail decoding are sent to
// orders.commands.dlq so the partition never blocks.
func (p *Processor) HandleRaw(ctx context.Context, part int32, offset int64, key, value []byte) error {
	p.mu.Lock()
	st, ok := p.partitions[part]
	if !ok {
		st = &partState{eng: engine.New()}
		p.partitions[part] = st
	}
	p.mu.Unlock()

	var wireCmd events.OrderCommand
	env, err := events.Unwrap(value, &wireCmd)
	if err != nil {
		return p.sendDLQ(ctx, key, value, err)
	}
	cmd, err := wireCmd.ToEngine()
	if err != nil {
		return p.sendDLQ(ctx, key, value, err)
	}

	t0 := time.Now()
	outEvents := st.eng.Process(cmd)
	obs.MatchLatencySeconds.Observe(time.Since(t0).Seconds())
	msgs := make([]outbox.Message, 0, len(outEvents))
	for _, ev := range outEvents {
		if ev.Kind == engine.EvDuplicate {
			continue
		}
		oev, tev := events.FromEngineEvent(ev)
		if tev != nil {
			obs.TradesTotal.WithLabelValues(tev.Symbol).Inc()
			raw, err := events.Wrap(tev.MatchID, events.TypeTradeExecuted, "matching-engine", env.CorrelationID, tev.ExecutedAt, tev)
			if err != nil {
				return err
			}
			msgs = append(msgs, outbox.Message{
				Topic:   kafka.TopicTrades,
				Key:     []byte(tev.Symbol),
				Payload: raw,
				Headers: map[string]string{
					"correlation_id": env.CorrelationID,
					"event_type":     events.TypeTradeExecuted,
				},
			})
		}
		if oev != nil {
			obs.OrdersTotal.WithLabelValues(oev.Side, oev.OrderType, oev.Status).Inc()
			raw, err := events.Wrap(oev.EventID, events.TypeOrderEvent, "matching-engine", env.CorrelationID, oev.Timestamp, oev)
			if err != nil {
				return err
			}
			msgs = append(msgs, outbox.Message{
				Topic:   kafka.TopicOrderEvents,
				Key:     []byte(oev.OrderID.String()),
				Payload: raw,
				Headers: map[string]string{
					"correlation_id": env.CorrelationID,
					"event_type":     events.TypeOrderEvent,
				},
			})
		}
	}

	if len(msgs) > 0 {
		if err := p.Pub.Publish(ctx, msgs); err != nil {
			return fmt.Errorf("publish engine events: %w", err)
		}
	}

	// Update cached depth snapshot for HTTP queries.
	if bk := st.eng.Book(cmd.Symbol); bk != nil {
		bids, asks := bk.Depth(50)
		p.mu.Lock()
		p.books[cmd.Symbol] = DepthSnapshot{
			Symbol: cmd.Symbol,
			Seq:    bk.Seq(),
			Bids:   bids,
			Asks:   asks,
		}
		p.mu.Unlock()
	}

	st.nextOffset = offset + 1
	st.cmdsSince++
	if p.Snapshots != nil && p.SnapshotEvery > 0 && st.cmdsSince >= p.SnapshotEvery {
		if err := p.SaveSnapshot(ctx, part); err != nil {
			p.Log.Warn("failed to save engine snapshot", "partition", part, "err", err)
		}
	}
	return nil
}

// SaveSnapshot persists the current state of partition part.
func (p *Processor) SaveSnapshot(ctx context.Context, part int32) error {
	p.mu.Lock()
	st, ok := p.partitions[part]
	if !ok {
		p.mu.Unlock()
		return nil
	}
	blob, err := st.eng.Snapshot()
	nextOff := st.nextOffset
	st.cmdsSince = 0
	p.mu.Unlock()
	if err != nil {
		return err
	}
	return p.Snapshots.Save(ctx, part, nextOff, blob)
}

// Depth returns the cached depth snapshot for symbol up to limit levels.
func (p *Processor) Depth(symbol string, limit int) DepthSnapshot {
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	p.mu.RLock()
	snap, ok := p.books[symbol]
	p.mu.RUnlock()
	if !ok {
		return DepthSnapshot{Symbol: symbol, Bids: []engine.Level{}, Asks: []engine.Level{}}
	}
	bids := snap.Bids
	if len(bids) > limit {
		bids = bids[:limit]
	}
	asks := snap.Asks
	if len(asks) > limit {
		asks = asks[:limit]
	}
	return DepthSnapshot{Symbol: symbol, Seq: snap.Seq, Bids: bids, Asks: asks}
}

func (p *Processor) sendDLQ(ctx context.Context, key, value []byte, cause error) error {
	p.Log.Error("routing poison command to DLQ", "err", cause)
	return p.Pub.Publish(ctx, []outbox.Message{{
		Topic:   kafka.DLQ(kafka.TopicOrderCommands),
		Key:     key,
		Payload: value,
		Headers: map[string]string{"error": cause.Error()},
	}})
}

// RunKafkaLoop consumes orders.commands from cl, processes each partition in
// order, and commits offsets after outbound events are published.
func (p *Processor) RunKafkaLoop(ctx context.Context, cl *kgo.Client) error {
	for {
		fetches := cl.PollFetches(ctx)
		if fetches.IsClientClosed() || ctx.Err() != nil {
			return ctx.Err()
		}
		fetches.EachError(func(t string, part int32, err error) {
			p.Log.Warn("kafka fetch error", "topic", t, "partition", part, "err", err)
		})

		fetches.EachPartition(func(fp kgo.FetchTopicPartition) {
			if len(fp.Records) == 0 {
				return
			}
			for _, rec := range fp.Records {
				for {
					err := p.HandleRaw(ctx, rec.Partition, rec.Offset, rec.Key, rec.Value)
					if err == nil {
						break
					}
					if ctx.Err() != nil {
						return
					}
					p.Log.Error("matching handle failed, retrying", "partition", rec.Partition, "offset", rec.Offset, "err", err)
					time.Sleep(200 * time.Millisecond)
				}
				if err := cl.CommitRecords(ctx, rec); err != nil && ctx.Err() == nil {
					p.Log.Warn("commit offset failed", "partition", rec.Partition, "offset", rec.Offset, "err", err)
				}
			}
		})
	}
}

// NewRouter exposes GET /api/v1/book/{symbol} alongside baseMux.
func NewRouter(p *Processor, baseMux *http.ServeMux) http.Handler {
	r := chi.NewRouter()
	r.Use(httpx.CorrelationMiddleware)
	r.Get("/api/v1/book/{symbol}", func(w http.ResponseWriter, r *http.Request) {
		sym := chi.URLParam(r, "symbol")
		depth, _ := strconv.Atoi(r.URL.Query().Get("depth"))
		httpx.WriteJSON(w, http.StatusOK, p.Depth(sym, depth))
	})
	if baseMux != nil {
		r.Mount("/", baseMux)
	}
	return r
}
