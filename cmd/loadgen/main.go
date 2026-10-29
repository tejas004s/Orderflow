// Command loadgen runs throughput, latency (p50/p95/p99), and chaos recovery
// benchmarks against OrderFlow (SDD sections 11, 14).
//
// Modes:
//
//	loadgen -engine-only -orders 200000
//	    Measures matching-engine throughput, p50/p95/p99 match latency, and
//	    snapshot crash-recovery latency in-process.
//
//	loadgen -gateway http://localhost:8080 -users 16 -orders 5000
//	    Drives concurrent HTTP order placement against a live OrderFlow stack.
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/tejas004s/orderflow/internal/engine"
	"github.com/tejas004s/orderflow/internal/events"
	"github.com/tejas004s/orderflow/internal/matching"
	"github.com/tejas004s/orderflow/internal/outbox"
)

type noopPub struct {
	trades atomic.Int64
	events atomic.Int64
}

func (p *noopPub) Publish(_ context.Context, msgs []outbox.Message) error {
	for _, m := range msgs {
		if m.Topic == "trades.executed" {
			p.trades.Add(1)
		} else {
			p.events.Add(1)
		}
	}
	return nil
}

type memSnapStore struct {
	mu   sync.Mutex
	off  int64
	blob []byte
}

func (s *memSnapStore) Load(_ context.Context, _ int32) (int64, []byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.blob) == 0 {
		return 0, nil, false, nil
	}
	return s.off, s.blob, true, nil
}

func (s *memSnapStore) Save(_ context.Context, _ int32, off int64, blob []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.off = off
	s.blob = append([]byte(nil), blob...)
	return nil
}

func main() {
	gatewayURL := flag.String("gateway", "http://localhost:8080", "API gateway base URL")
	users := flag.Int("users", 16, "concurrent virtual users for HTTP load test")
	orders := flag.Int("orders", 100000, "total orders to submit")
	engineOnly := flag.Bool("engine-only", false, "run in-process matching + crash-recovery benchmark")
	flag.Parse()

	if *engineOnly {
		runEngineAndChaosBench(*orders)
		return
	}
	if err := runHTTPLoad(*gatewayURL, *users, *orders); err != nil {
		fmt.Fprintln(os.Stderr, "loadgen:", err)
		os.Exit(1)
	}
}

func runEngineAndChaosBench(n int) {
	silent := slog.New(slog.NewTextHandler(io.Discard, nil))
	pub := &noopPub{}
	snaps := &memSnapStore{}
	proc := matching.NewProcessor(pub, snaps, silent)
	proc.SnapshotEvery = int64(max(n/10, 1000))

	rng := rand.New(rand.NewPCG(101, 202))
	uIDs := [8]uuid.UUID{
		uuid.New(), uuid.New(), uuid.New(), uuid.New(),
		uuid.New(), uuid.New(), uuid.New(), uuid.New(),
	}

	rawCmds := make([][]byte, n)
	for i := 0; i < n; i++ {
		var cid, oid [16]byte
		binary.BigEndian.PutUint64(cid[8:], uint64(i+1))
		binary.BigEndian.PutUint64(oid[:8], 9)
		binary.BigEndian.PutUint64(oid[8:], uint64(i+1))
		side := "BUY"
		if i%2 == 1 {
			side = "SELL"
		}
		px := int64(18800 + rng.IntN(201))
		qty := int64(rng.IntN(30) + 1)
		wc := events.OrderCommand{
			CommandID: uuid.UUID(cid),
			Kind:      "PLACE",
			Symbol:    "AAPL",
			OrderID:   uuid.UUID(oid),
			UserID:    uIDs[i%len(uIDs)],
			Side:      side,
			OrderType: "LIMIT",
			Price:     px,
			Qty:       qty,
			Timestamp: int64(i+1) * 1000,
		}
		rawCmds[i], _ = events.Wrap(wc.CommandID, events.TypeOrderCommand, "loadgen", "", wc.Timestamp, wc)
	}

	ctx := context.Background()
	latencies := make([]time.Duration, n)
	start := time.Now()
	killAt := (n * 7) / 10 // simulate kill -9 at 70% of stream
	var recoveryTime time.Duration

	for i := 0; i < n; i++ {
		if i == killAt {
			// Simulate process crash and restart from last saved snapshot!
			recStart := time.Now()
			proc = matching.NewProcessor(pub, snaps, silent)
			proc.SnapshotEvery = int64(max(n/10, 1000))
			nextOff, err := proc.InitPartition(ctx, 0)
			if err != nil {
				panic(err)
			}
			for j := int(nextOff); j < i; j++ {
				if err := proc.HandleRaw(ctx, 0, int64(j), []byte("AAPL"), rawCmds[j]); err != nil {
					panic(err)
				}
			}
			recoveryTime = time.Since(recStart)
		}
		t0 := time.Now()
		if err := proc.HandleRaw(ctx, 0, int64(i), []byte("AAPL"), rawCmds[i]); err != nil {
			panic(err)
		}
		latencies[i] = time.Since(t0)
	}
	elapsed := time.Since(start)
	slices.Sort(latencies)

	// Also measure pure engine.Process latency without JSON envelope wrapping.
	pureEng := engine.New()
	pureLat := make([]time.Duration, n)
	pureStart := time.Now()
	for i := 0; i < n; i++ {
		var cid, oid [16]byte
		binary.BigEndian.PutUint64(cid[8:], uint64(i+1))
		binary.BigEndian.PutUint64(oid[:8], 11)
		binary.BigEndian.PutUint64(oid[8:], uint64(i+1))
		side := engine.Buy
		if i%2 == 1 {
			side = engine.Sell
		}
		c := engine.Command{
			ID: uuid.UUID(cid), Kind: engine.CmdPlace, Symbol: "AAPL",
			OrderID: uuid.UUID(oid), UserID: uIDs[i%len(uIDs)],
			Side: side, Type: engine.Limit,
			Price: int64(18850 + (i%50)*2), Qty: int64((i % 20) + 1),
			Timestamp: int64(i+1) * 1000,
		}
		t0 := time.Now()
		pureEng.Process(c)
		pureLat[i] = time.Since(t0)
	}
	pureElapsed := time.Since(pureStart)
	slices.Sort(pureLat)

	fmt.Printf("=== OrderFlow Matching & Crash-Recovery Benchmark (%d orders) ===\n", n)
	fmt.Printf("Pure Engine Throughput:     %.0f orders/sec\n", float64(n)/pureElapsed.Seconds())
	fmt.Printf("Pure Match Latency:         p50=%v  p95=%v  p99=%v\n",
		pureLat[n*50/100], pureLat[n*95/100], pureLat[n*99/100])
	fmt.Printf("Full Processor Throughput:  %.0f orders/sec (envelope decode + match + event encode + snapshots)\n",
		float64(n)/elapsed.Seconds())
	fmt.Printf("Processor Latency:          p50=%v  p95=%v  p99=%v\n",
		latencies[n*50/100], latencies[n*95/100], latencies[n*99/100])
	fmt.Printf("Crash Recovery Time:        %v (restored snapshot + replayed tail)\n", recoveryTime)
}

func runHTTPLoad(gatewayURL string, numUsers, totalOrders int) error {
	ctx := context.Background()
	cl := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        256,
			MaxIdleConnsPerHost: 256,
		},
	}

	tokens := make([]string, numUsers)
	for i := 0; i < numUsers; i++ {
		email := fmt.Sprintf("loaduser-%d-%d@orderflow.local", time.Now().Unix(), i)
		raw, _ := json.Marshal(map[string]string{"email": email, "password": "password123"})
		resp, err := cl.Post(gatewayURL+"/api/v1/auth/register", "application/json", bytes.NewReader(raw))
		if err != nil {
			return fmt.Errorf("register user %d: %w", i, err)
		}
		var pair struct {
			AccessToken string `json:"access_token"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&pair)
		resp.Body.Close()
		tokens[i] = pair.AccessToken

		seedBody, _ := json.Marshal(map[string]any{"cash_cents": 500_000_000, "symbol": "AAPL", "shares": 100_000})
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, gatewayURL+"/api/v1/portfolio/seed", bytes.NewReader(seedBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+tokens[i])
		if r, err := cl.Do(req); err == nil {
			r.Body.Close()
		}
	}

	latencies := make([]time.Duration, totalOrders)
	var idx atomic.Int64
	var wg sync.WaitGroup
	start := time.Now()

	for w := 0; w < numUsers; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			tok := tokens[worker]
			for {
				i := int(idx.Add(1) - 1)
				if i >= totalOrders {
					return
				}
				side := "BUY"
				if i%2 == 1 {
					side = "SELL"
				}
				px := 18850 + (i%40)*5
				body, _ := json.Marshal(map[string]any{
					"symbol": "AAPL", "side": side, "type": "LIMIT", "price": px, "qty": 2,
				})
				req, _ := http.NewRequestWithContext(ctx, http.MethodPost, gatewayURL+"/api/v1/orders", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer "+tok)
				req.Header.Set("Idempotency-Key", fmt.Sprintf("lg-%d-%d", worker, i))

				t0 := time.Now()
				resp, err := cl.Do(req)
				latencies[i] = time.Since(t0)
				if err == nil {
					_, _ = io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
			}
		}(w)
	}
	wg.Wait()
	elapsed := time.Since(start)
	slices.Sort(latencies)

	fmt.Printf("=== OrderFlow HTTP Load Report (%d orders, %d concurrent users) ===\n", totalOrders, numUsers)
	fmt.Printf("Total Elapsed: %v\n", elapsed)
	fmt.Printf("Throughput:    %.1f orders/sec\n", float64(totalOrders)/elapsed.Seconds())
	fmt.Printf("HTTP Latency:  p50=%v  p95=%v  p99=%v\n",
		latencies[totalOrders*50/100], latencies[totalOrders*95/100], latencies[totalOrders*99/100])
	return nil
}
