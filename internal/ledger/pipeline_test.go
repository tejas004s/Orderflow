package ledger_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tejas004s/orderflow/internal/events"
	"github.com/tejas004s/orderflow/internal/kafka"
	"github.com/tejas004s/orderflow/internal/ledger"
	"github.com/tejas004s/orderflow/internal/matching"
	"github.com/tejas004s/orderflow/internal/order"
	"github.com/tejas004s/orderflow/internal/outbox"
	"github.com/tejas004s/orderflow/internal/testpg"
)

// memBus captures published outbox/Kafka messages in memory so tests can
// inspect, duplicate, or reorder them deterministically.
type memBus struct {
	msgs []outbox.Message
}

func (b *memBus) Publish(_ context.Context, msgs []outbox.Message) error {
	for _, m := range msgs {
		cp := outbox.Message{
			ID:      m.ID,
			Topic:   m.Topic,
			Key:     append([]byte(nil), m.Key...),
			Payload: append([]byte(nil), m.Payload...),
			Headers: m.Headers,
		}
		b.msgs = append(b.msgs, cp)
	}
	return nil
}

func (b *memBus) takeAll() []outbox.Message {
	out := b.msgs
	b.msgs = nil
	return out
}

func seedUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool, email string, cashCents int64, symbol string, shares int64, avgCost int64) uuid.UUID {
	t.Helper()
	var uid uuid.UUID
	err := pool.QueryRow(ctx, `
		INSERT INTO users (email, password_hash)
		VALUES ($1, 'hash')
		RETURNING id
	`, email).Scan(&uid)
	if err != nil {
		t.Fatalf("insert user %s: %v", email, err)
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO accounts (user_id, currency, available, reserved)
		VALUES ($1, 'USD', $2, 0)
	`, uid, cashCents)
	if err != nil {
		t.Fatalf("insert account %s: %v", email, err)
	}
	if symbol != "" && shares > 0 {
		_, err = pool.Exec(ctx, `
			INSERT INTO positions (user_id, symbol, qty, reserved_qty, avg_cost)
			VALUES ($1, $2, $3, 0, $4)
		`, uid, symbol, shares, avgCost)
		if err != nil {
			t.Fatalf("insert position %s: %v", email, err)
		}
	}
	return uid
}

func TestEndToEndOrderOutboxEngineLedger(t *testing.T) {
	pool, _ := testpg.New(t)
	ctx := context.Background()
	silentLog := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Seed Alice with $10,000.00 (1,000,000 cents) and Bob with 100 AAPL shares.
	alice := seedUser(t, ctx, pool, "alice@example.com", 1_000_000, "", 0, 0)
	bob := seedUser(t, ctx, pool, "bob@example.com", 0, "AAPL", 100, 15000)

	cmdBus := &memBus{}
	relay := outbox.NewRelay(pool, cmdBus, silentLog)
	ordSvc := order.NewService(pool, relay)
	srv := httptest.NewServer(order.NewRouter(ordSvc, nil))
	defer srv.Close()

	engineBus := &memBus{}
	proc := matching.NewProcessor(engineBus, matching.PGSnapshotStore{Pool: pool}, silentLog)
	ledgerSvc := ledger.NewService(pool, nil, silentLog)

	// 1. Bob places a resting SELL LIMIT for 10 AAPL @ $185.00 (18500 cents) over HTTP.
	placeHTTP := func(uid uuid.UUID, idemKey string, body map[string]any) (*http.Response, []byte) {
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/orders", bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(order.HeaderUserID, uid.String())
		req.Header.Set("Idempotency-Key", idemKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("http post: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, b
	}

	resp, body := placeHTTP(bob, "bob-sell-1", map[string]any{
		"symbol": "AAPL", "side": "SELL", "type": "LIMIT", "price": 18500, "qty": 10,
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("bob sell status=%d body=%s", resp.StatusCode, body)
	}
	var bobOrder order.Order
	_ = json.Unmarshal(body, &bobOrder)

	// Idempotent replay with identical key + body -> 200 OK, same order ID.
	respReplay, bodyReplay := placeHTTP(bob, "bob-sell-1", map[string]any{
		"symbol": "AAPL", "side": "SELL", "type": "LIMIT", "price": 18500, "qty": 10,
	})
	if respReplay.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on idempotent replay, got %d: %s", respReplay.StatusCode, bodyReplay)
	}

	// Idempotency conflict: same key, different qty -> 409 Conflict RFC 7807.
	respConflict, _ := placeHTTP(bob, "bob-sell-1", map[string]any{
		"symbol": "AAPL", "side": "SELL", "type": "LIMIT", "price": 18500, "qty": 20,
	})
	if respConflict.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 on idempotency conflict, got %d", respConflict.StatusCode)
	}

	// Insufficient funds check -> 422 Unprocessable Entity.
	respInsuff, _ := placeHTTP(alice, "alice-too-big", map[string]any{
		"symbol": "AAPL", "side": "BUY", "type": "LIMIT", "price": 20000, "qty": 500,
	})
	if respInsuff.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 on insufficient funds, got %d", respInsuff.StatusCode)
	}

	// 2. Alice places an aggressive BUY LIMIT for 10 AAPL @ $190.00 (19000 cents).
	// Should match at Bob's resting price of $185.00 (18500 cents), giving Alice
	// $5.00/share ($50.00 = 5000 cents) price improvement!
	resp, body = placeHTTP(alice, "alice-buy-1", map[string]any{
		"symbol": "AAPL", "side": "BUY", "type": "LIMIT", "price": 19000, "qty": 10,
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("alice buy status=%d body=%s", resp.StatusCode, body)
	}
	var aliceOrder order.Order
	_ = json.Unmarshal(body, &aliceOrder)

	// Reconcile while orders are still NEW / reserved in DB (before engine runs).
	if _, err := ledger.Reconcile(ctx, pool); err != nil {
		t.Fatalf("pre-settlement reconcile failed: %v", err)
	}

	// 3. Drain outbox -> matching engine -> ledger.
	n, err := relay.DrainBatch(ctx)
	if err != nil || n != 2 {
		t.Fatalf("expected 2 outbox messages drained, got %d (err=%v)", n, err)
	}
	for i, m := range cmdBus.takeAll() {
		if m.Topic != kafka.TopicOrderCommands {
			t.Fatalf("unexpected command topic %s", m.Topic)
		}
		if err := proc.HandleRaw(ctx, 0, int64(i), m.Key, m.Payload); err != nil {
			t.Fatalf("engine HandleRaw: %v", err)
		}
	}

	outMsgs := engineBus.takeAll()
	var tradeCount int
	for _, m := range outMsgs {
		if m.Topic == kafka.TopicTrades {
			tradeCount++
		}
		if err := ledgerSvc.HandleRaw(ctx, m.Topic, m.Key, m.Payload); err != nil {
			t.Fatalf("ledger HandleRaw(%s): %v", m.Topic, err)
		}
	}
	if tradeCount != 1 {
		t.Fatalf("expected 1 message in trades.executed, got %d", tradeCount)
	}

	// 4. Verify balances, positions, order statuses, and double-entry reconciliation.
	var aliceAvail, aliceRes int64
	_ = pool.QueryRow(ctx, `SELECT available, reserved FROM accounts WHERE user_id = $1`, alice).
		Scan(&aliceAvail, &aliceRes)
	// Alice started with 1,000,000, reserved 190,000, spent 185,000, got 5,000 price improvement back -> 815,000 available, 0 reserved.
	if aliceAvail != 815_000 || aliceRes != 0 {
		t.Fatalf("alice cash: got available=%d reserved=%d, want 815000 / 0", aliceAvail, aliceRes)
	}

	var bobAvail, bobRes int64
	_ = pool.QueryRow(ctx, `SELECT available, reserved FROM accounts WHERE user_id = $1`, bob).
		Scan(&bobAvail, &bobRes)
	if bobAvail != 185_000 || bobRes != 0 {
		t.Fatalf("bob cash: got available=%d reserved=%d, want 185000 / 0", bobAvail, bobRes)
	}

	var aliceShares, aliceAvg int64
	_ = pool.QueryRow(ctx, `SELECT qty, avg_cost FROM positions WHERE user_id = $1 AND symbol = 'AAPL'`, alice).
		Scan(&aliceShares, &aliceAvg)
	if aliceShares != 10 || aliceAvg != 18500 {
		t.Fatalf("alice position: qty=%d avg_cost=%d, want 10 @ 18500", aliceShares, aliceAvg)
	}

	var bobShares, bobResShares int64
	_ = pool.QueryRow(ctx, `SELECT qty, reserved_qty FROM positions WHERE user_id = $1 AND symbol = 'AAPL'`, bob).
		Scan(&bobShares, &bobResShares)
	if bobShares != 90 || bobResShares != 0 {
		t.Fatalf("bob position: qty=%d reserved_qty=%d, want 90 / 0", bobShares, bobResShares)
	}

	ordA, _ := ordSvc.GetOrder(ctx, alice, aliceOrder.ID)
	ordB, _ := ordSvc.GetOrder(ctx, bob, bobOrder.ID)
	if ordA.Status != "FILLED" || ordA.FilledQty != 10 || ordB.Status != "FILLED" || ordB.FilledQty != 10 {
		t.Fatalf("orders not FILLED: alice=%+v bob=%+v", ordA, ordB)
	}

	rep, err := ledger.Reconcile(ctx, pool)
	if err != nil {
		t.Fatalf("post-settlement reconcile failed: %v", err)
	}
	if rep.TransactionsChecked != 1 || rep.TotalCashSupply != 1_000_000 || rep.TotalReservedCash != 0 {
		t.Fatalf("unexpected audit report: %+v", rep)
	}
}

func TestDuplicateAndOutOfOrderEventDelivery(t *testing.T) {
	// Run the exact same scenario under two delivery schedules:
	//   Schedule 0: normal order, each event delivered once
	//   Schedule 1: orders.events delivered BEFORE trades.executed, and EVERY event delivered TWICE
	// Both must pass Reconcile and produce identical final database state.
	runScenario := func(duplicateAndReorder bool) string {
		pool, _ := testpg.New(t)
		ctx := context.Background()
		silentLog := slog.New(slog.NewTextHandler(io.Discard, nil))

		alice := seedUser(t, ctx, pool, "alice@example.com", 5_000_000, "AAPL", 200, 18000)
		bob := seedUser(t, ctx, pool, "bob@example.com", 5_000_000, "AAPL", 200, 18000)

		cmdBus := &memBus{}
		relay := outbox.NewRelay(pool, cmdBus, silentLog)
		ordSvc := order.NewService(pool, relay)
		engineBus := &memBus{}
		proc := matching.NewProcessor(engineBus, nil, silentLog)
		ledgerSvc := ledger.NewService(pool, nil, silentLog)

		// 1. Bob posts two sell limits: 5 @ 18500 and 10 @ 18600.
		_, _, err := ordSvc.PlaceOrder(ctx, order.PlaceRequest{
			UserID: bob, IdempotencyKey: "b1", Symbol: "AAPL", Side: "SELL", Type: "LIMIT", Price: 18500, Qty: 5,
		})
		if err != nil {
			t.Fatal(err)
		}
		bobOrd2, _, err := ordSvc.PlaceOrder(ctx, order.PlaceRequest{
			UserID: bob, IdempotencyKey: "b2", Symbol: "AAPL", Side: "SELL", Type: "LIMIT", Price: 18600, Qty: 10,
		})
		if err != nil {
			t.Fatal(err)
		}
		// 2. Alice posts a buy limit for 8 @ 18700 (sweeps 5 @ 18500 and 3 @ 18600, with price improvement on both).
		_, _, err = ordSvc.PlaceOrder(ctx, order.PlaceRequest{
			UserID: alice, IdempotencyKey: "a1", Symbol: "AAPL", Side: "BUY", Type: "LIMIT", Price: 18700, Qty: 8,
		})
		if err != nil {
			t.Fatal(err)
		}
		// 3. Bob cancels the remaining 7 shares of bobOrd2.
		if _, err := ordSvc.CancelOrder(ctx, bob, bobOrd2.ID); err != nil {
			t.Fatal(err)
		}
		// 4. Alice places a market buy for 4 shares (book is now empty -> rejected with NO_LIQUIDITY, reservation released).
		_, _, err = ordSvc.PlaceOrder(ctx, order.PlaceRequest{
			UserID: alice, IdempotencyKey: "a2", Symbol: "AAPL", Side: "BUY", Type: "MARKET", Qty: 4,
		})
		if err != nil {
			t.Fatal(err)
		}

		if _, err := relay.DrainBatch(ctx); err != nil {
			t.Fatal(err)
		}
		for i, m := range cmdBus.takeAll() {
			if err := proc.HandleRaw(ctx, 0, int64(i), m.Key, m.Payload); err != nil {
				t.Fatal(err)
			}
		}

		evMsgs := engineBus.takeAll()
		if duplicateAndReorder {
			// Deliver all orders.events BEFORE trades.executed, and deliver every message twice!
			var orderEvs, tradeEvs []outbox.Message
			for _, m := range evMsgs {
				if m.Topic == kafka.TopicOrderEvents {
					orderEvs = append(orderEvs, m)
				} else {
					tradeEvs = append(tradeEvs, m)
				}
			}
			evMsgs = nil
			for _, m := range orderEvs {
				evMsgs = append(evMsgs, m, m)
			}
			for _, m := range tradeEvs {
				evMsgs = append(evMsgs, m, m)
			}
		}

		for _, m := range evMsgs {
			if err := ledgerSvc.HandleRaw(ctx, m.Topic, m.Key, m.Payload); err != nil {
				t.Fatalf("HandleRaw(%s): %v", m.Topic, err)
			}
		}

		if _, err := ledger.Reconcile(ctx, pool); err != nil {
			t.Fatalf("reconcile (dup=%v): %v", duplicateAndReorder, err)
		}

		// Build canonical state digest of accounts, positions, orders, and ledger net.
		var buf bytes.Buffer
		rows, _ := pool.Query(ctx, `
			SELECT u.email, a.available, a.reserved, p.qty, p.reserved_qty, p.avg_cost
			FROM users u
			JOIN accounts a ON a.user_id = u.id
			JOIN positions p ON p.user_id = u.id
			ORDER BY u.email
		`)
		for rows.Next() {
			var email string
			var av, res, q, rq, avg int64
			_ = rows.Scan(&email, &av, &res, &q, &rq, &avg)
			fmt.Fprintf(&buf, "user=%s avail=%d res=%d qty=%d rqty=%d avg=%d\n", email, av, res, q, rq, avg)
		}
		rows.Close()

		rows, _ = pool.Query(ctx, `
			SELECT side, type, qty, filled_qty, reserved_amount, status, COALESCE(reject_reason, '')
			FROM orders ORDER BY created_at, id
		`)
		for rows.Next() {
			var side, typ, st, reason string
			var q, fq, ra int64
			_ = rows.Scan(&side, &typ, &q, &fq, &ra, &st, &reason)
			fmt.Fprintf(&buf, "order %s %s qty=%d filled=%d res=%d st=%s reason=%s\n", side, typ, q, fq, ra, st, reason)
		}
		rows.Close()
		return buf.String()
	}

	normal := runScenario(false)
	dupReordered := runScenario(true)
	if normal != dupReordered {
		t.Fatalf("state mismatch under duplicate/reordered delivery:\n--- normal ---\n%s\n--- dup+reordered ---\n%s",
			normal, dupReordered)
	}
}

func TestMatchingEngineSnapshotAndReplayRecovery(t *testing.T) {
	pool, _ := testpg.New(t)
	ctx := context.Background()
	silentLog := slog.New(slog.NewTextHandler(io.Discard, nil))

	alice := seedUser(t, ctx, pool, "alice@example.com", 5_000_000, "AAPL", 200, 18000)
	bob := seedUser(t, ctx, pool, "bob@example.com", 5_000_000, "AAPL", 200, 18000)

	cmdBus := &memBus{}
	relay := outbox.NewRelay(pool, cmdBus, silentLog)
	ordSvc := order.NewService(pool, relay)

	for i := 0; i < 6; i++ {
		side := "SELL"
		uid := bob
		if i%2 == 1 {
			side = "BUY"
			uid = alice
		}
		_, _, err := ordSvc.PlaceOrder(ctx, order.PlaceRequest{
			UserID: uid, IdempotencyKey: fmt.Sprintf("k-%d", i),
			Symbol: "AAPL", Side: side, Type: "LIMIT", Price: 18500, Qty: 5,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := relay.DrainBatch(ctx); err != nil {
		t.Fatal(err)
	}
	cmds := cmdBus.takeAll()

	engineBus := &memBus{}
	snaps := matching.PGSnapshotStore{Pool: pool}
	proc1 := matching.NewProcessor(engineBus, snaps, silentLog)
	proc1.SnapshotEvery = 3 // snapshot after offset 2 (next_offset = 3)

	// Process first 4 commands (offsets 0, 1, 2, 3), then simulate a crash!
	for i := 0; i < 4; i++ {
		if err := proc1.HandleRaw(ctx, 0, int64(i), cmds[i].Key, cmds[i].Payload); err != nil {
			t.Fatal(err)
		}
	}

	// Restart a fresh Processor, load snapshot (which was saved at next_offset = 3),
	// and replay from next_offset = 3 through the end (offsets 3, 4, 5).
	proc2 := matching.NewProcessor(engineBus, snaps, silentLog)
	nextOff, err := proc2.InitPartition(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if nextOff != 3 {
		t.Fatalf("expected snapshot next_offset=3, got %d", nextOff)
	}
	for i := int(nextOff); i < len(cmds); i++ {
		if err := proc2.HandleRaw(ctx, 0, int64(i), cmds[i].Key, cmds[i].Payload); err != nil {
			t.Fatal(err)
		}
	}

	// Feed all emitted events (including the duplicate for offset 3!) to ledger.
	ledgerSvc := ledger.NewService(pool, nil, silentLog)
	for _, m := range engineBus.takeAll() {
		var tr events.TradeExecuted
		if env, err := events.Unwrap(m.Payload, &tr); err == nil && env.EventType == events.TypeTradeExecuted {
			_ = tr
		}
		if err := ledgerSvc.HandleRaw(ctx, m.Topic, m.Key, m.Payload); err != nil {
			t.Fatal(err)
		}
	}

	var tradeRows int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM trades`).Scan(&tradeRows)
	if tradeRows != 3 {
		t.Fatalf("expected exactly 3 trades after crash recovery, got %d", tradeRows)
	}
	if _, err := ledger.Reconcile(ctx, pool); err != nil {
		t.Fatalf("reconcile after crash recovery failed: %v", err)
	}
}

func TestLedgerConservationAndReconcileRandomized(t *testing.T) {
	pool, _ := testpg.New(t)
	ctx := context.Background()
	silentLog := slog.New(slog.NewTextHandler(io.Discard, nil))

	const initialCashPerUser = 10_000_000 // $100,000.00
	const initialSharesPerUser = 500
	users := make([]uuid.UUID, 4)
	for i := range users {
		users[i] = seedUser(t, ctx, pool, fmt.Sprintf("u%d@example.com", i), initialCashPerUser, "AAPL", initialSharesPerUser, 18500)
	}

	cmdBus := &memBus{}
	relay := outbox.NewRelay(pool, cmdBus, silentLog)
	ordSvc := order.NewService(pool, relay)
	engineBus := &memBus{}
	proc := matching.NewProcessor(engineBus, matching.PGSnapshotStore{Pool: pool}, silentLog)
	ledgerSvc := ledger.NewService(pool, nil, silentLog)

	var placed []order.Order
	for step := 1; step <= 60; step++ {
		uid := users[step%len(users)]
		if step%7 == 0 && len(placed) > 0 {
			pick := placed[(step*3)%len(placed)]
			_, _ = ordSvc.CancelOrder(ctx, pick.UserID, pick.ID)
		} else {
			side := "BUY"
			if step%2 == 1 {
				side = "SELL"
			}
			otype := "LIMIT"
			px := int64(18400 + (step%7)*50)
			if step%9 == 0 {
				otype = "MARKET"
				px = 0
			}
			qty := int64((step % 12) + 1)
			ord, _, err := ordSvc.PlaceOrder(ctx, order.PlaceRequest{
				UserID: uid, IdempotencyKey: fmt.Sprintf("step-%d", step),
				Symbol: "AAPL", Side: side, Type: otype, Price: px, Qty: qty,
			})
			if err == nil {
				placed = append(placed, ord)
			}
		}

		if _, err := relay.DrainBatch(ctx); err != nil {
			t.Fatal(err)
		}
		for i, m := range cmdBus.takeAll() {
			if err := proc.HandleRaw(ctx, 0, int64(step*100+i), m.Key, m.Payload); err != nil {
				t.Fatal(err)
			}
		}
		for _, m := range engineBus.takeAll() {
			// Deliver every event twice to stress idempotency on every step.
			if err := ledgerSvc.HandleRaw(ctx, m.Topic, m.Key, m.Payload); err != nil {
				t.Fatal(err)
			}
			if err := ledgerSvc.HandleRaw(ctx, m.Topic, m.Key, m.Payload); err != nil {
				t.Fatal(err)
			}
		}
	}

	rep, err := ledger.Reconcile(ctx, pool)
	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}
	wantCash := int64(len(users)) * initialCashPerUser
	if rep.TotalCashSupply != wantCash {
		t.Fatalf("cash conservation violated: got %d want %d", rep.TotalCashSupply, wantCash)
	}

	var totalShares int64
	_ = pool.QueryRow(ctx, `SELECT COALESCE(SUM(qty + reserved_qty), 0) FROM positions WHERE symbol = 'AAPL'`).Scan(&totalShares)
	wantShares := int64(len(users)) * initialSharesPerUser
	if totalShares != wantShares {
		t.Fatalf("share conservation violated: got %d want %d", totalShares, wantShares)
	}
}
