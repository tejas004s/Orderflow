// Package bots implements market-maker and noise-trader bots that trade against
// api-gateway over REST so the exchange has deep order books and a live trade
// tape out of the box (SDD section 10).
package bots

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

type botIdentity struct {
	email string
	token string
}

type instrumentInfo struct {
	Symbol    string `json:"symbol"`
	TickSize  int64  `json:"tick_size"`
	RefPrice  int64  `json:"ref_price"`
	LastPrice int64  `json:"last_price"`
}

// Runner drives market-maker and noise-trader bots against GatewayURL.
type Runner struct {
	GatewayURL string
	Interval   time.Duration
	Log        *slog.Logger
	Client     *http.Client

	mmBuy   botIdentity
	mmSell  botIdentity
	noiseA  botIdentity
	noiseB  botIdentity
	symbols []instrumentInfo
}

// NewRunner constructs a bot Runner.
func NewRunner(gatewayURL string, log *slog.Logger) *Runner {
	return &Runner{
		GatewayURL: strings.TrimRight(gatewayURL, "/"),
		Interval:   600 * time.Millisecond,
		Log:        log,
		Client:     &http.Client{Timeout: 5 * time.Second},
	}
}

// Run initializes the bot accounts and loops placing orders until ctx ends.
func (r *Runner) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		if err := r.bootstrap(ctx); err != nil {
			r.Log.Warn("bot bootstrap waiting for gateway", "err", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
			continue
		}
		break
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	r.Log.Info("bots initialized", "symbols", len(r.symbols))

	// Place initial bid/ask ladder for every symbol immediately.
	rng := rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 77))
	for _, inst := range r.symbols {
		r.postLadder(ctx, inst)
	}

	ticker := time.NewTicker(r.Interval)
	defer ticker.Stop()
	step := 0
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			step++
			if step%15 == 0 {
				_ = r.refreshInstruments(ctx)
			}
			if len(r.symbols) == 0 {
				continue
			}
			inst := r.symbols[step%len(r.symbols)]
			if step%4 == 0 {
				r.postLadder(ctx, inst)
			}
			r.postNoiseOrder(ctx, rng, inst, step)
		}
	}
}

func (r *Runner) bootstrap(ctx context.Context) error {
	if err := r.refreshInstruments(ctx); err != nil {
		return err
	}
	var err error
	if r.mmBuy, err = r.ensureBot(ctx, "mm-bids@bots.orderflow.local"); err != nil {
		return err
	}
	if r.mmSell, err = r.ensureBot(ctx, "mm-asks@bots.orderflow.local"); err != nil {
		return err
	}
	if r.noiseA, err = r.ensureBot(ctx, "noise-alpha@bots.orderflow.local"); err != nil {
		return err
	}
	if r.noiseB, err = r.ensureBot(ctx, "noise-beta@bots.orderflow.local"); err != nil {
		return err
	}
	return nil
}

func (r *Runner) ensureBot(ctx context.Context, email string) (botIdentity, error) {
	const pass = "bot-secret-pass"
	tok, err := r.authCall(ctx, "/api/v1/auth/register", email, pass)
	if err != nil {
		tok, err = r.authCall(ctx, "/api/v1/auth/login", email, pass)
		if err != nil {
			return botIdentity{}, err
		}
	}
	id := botIdentity{email: email, token: tok}
	// Seed generous virtual cash + shares across all symbols so bots never run dry.
	_ = r.seed(ctx, id, 500_000_000, "", 0)
	for _, s := range r.symbols {
		_ = r.seed(ctx, id, 0, s.Symbol, 50_000)
	}
	return id, nil
}

func (r *Runner) authCall(ctx context.Context, path, email, password string) (string, error) {
	body, _ := json.Marshal(map[string]string{"email": email, "password": password})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.GatewayURL+path, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("%s status %d: %s", path, resp.StatusCode, b)
	}
	var res struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", err
	}
	return res.AccessToken, nil
}

func (r *Runner) seed(ctx context.Context, b botIdentity, cash int64, symbol string, shares int64) error {
	body, _ := json.Marshal(map[string]any{
		"cash_cents": cash,
		"symbol":     symbol,
		"shares":     shares,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.GatewayURL+"/api/v1/portfolio/seed", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+b.token)
	resp, err := r.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

func (r *Runner) refreshInstruments(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.GatewayURL+"/api/v1/instruments", nil)
	if err != nil {
		return err
	}
	resp, err := r.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("instruments status %d", resp.StatusCode)
	}
	var data struct {
		Instruments []instrumentInfo `json:"instruments"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return err
	}
	r.symbols = data.Instruments
	return nil
}

func (r *Runner) postLadder(ctx context.Context, inst instrumentInfo) {
	mid := inst.LastPrice
	if mid <= 0 {
		mid = inst.RefPrice
	}
	step := max(inst.TickSize*15, mid/1000) // ~0.1% per level
	for lvl := int64(1); lvl <= 5; lvl++ {
		bidPx := mid - lvl*step
		askPx := mid + lvl*step
		qty := int64(5 * lvl)
		if bidPx > 0 {
			_ = r.placeOrder(ctx, r.mmBuy, inst.Symbol, "BUY", "LIMIT", bidPx, qty)
		}
		_ = r.placeOrder(ctx, r.mmSell, inst.Symbol, "SELL", "LIMIT", askPx, qty)
	}
}

func (r *Runner) postNoiseOrder(ctx context.Context, rng *rand.Rand, inst instrumentInfo, step int) {
	mid := inst.LastPrice
	if mid <= 0 {
		mid = inst.RefPrice
	}
	bot := r.noiseA
	side := "BUY"
	if step%2 == 1 {
		bot = r.noiseB
		side = "SELL"
	}
	qty := int64(rng.IntN(8) + 1)
	if rng.IntN(4) == 0 {
		_ = r.placeOrder(ctx, bot, inst.Symbol, side, "MARKET", 0, qty)
		return
	}
	offset := int64(rng.IntN(41)-20) * max(inst.TickSize*5, 10)
	px := max(inst.TickSize, mid+offset)
	_ = r.placeOrder(ctx, bot, inst.Symbol, side, "LIMIT", px, qty)
}

func (r *Runner) placeOrder(ctx context.Context, b botIdentity, sym, side, otype string, price, qty int64) error {
	payload := map[string]any{
		"symbol": sym,
		"side":   side,
		"type":   otype,
		"qty":    qty,
	}
	if otype == "LIMIT" {
		payload["price"] = price
	}
	raw, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.GatewayURL+"/api/v1/orders", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+b.token)
	req.Header.Set("Idempotency-Key", uuid.NewString())
	resp, err := r.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}
