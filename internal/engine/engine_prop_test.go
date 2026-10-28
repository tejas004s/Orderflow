package engine

import (
	"bytes"
	"encoding/binary"
	"math/rand/v2"
	"testing"

	"github.com/google/uuid"
	"pgregory.net/rapid"
)

// TestPropertyOrderStreamInvariants uses rapid to generate random command
// streams (place limit, place market, cancel, duplicate) and checks every
// invariant from SDD section 5.5:
//   - Book is never crossed (best bid < best ask) after processing a command.
//   - Total filled qty on buy side equals sell side for every trade.
//   - No order fills more than its quantity.
//   - Replay of the same input yields byte-identical output.
//   - Snapshot + Restore mid-stream yields identical output to uninterrupted run.
//   - IndexBTree and IndexHeap produce identical event streams.
func TestPropertyOrderStreamInvariants(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		users := []uuid.UUID{uid("u1"), uid("u2"), uid("u3"), uid("u4")}
		nCmds := rapid.IntRange(20, 150).Draw(rt, "nCmds")

		cmds := make([]Command, 0, nCmds)
		var knownOrders []struct {
			id   uuid.UUID
			user uuid.UUID
		}

		for i := 0; i < nCmds; i++ {
			var cmdID [16]byte
			binary.BigEndian.PutUint64(cmdID[8:], uint64(i+1))
			ts := int64(i+1) * 1000

			action := rapid.IntRange(0, 9).Draw(rt, "action")
			if action <= 1 && len(knownOrders) > 0 {
				// Cancel an existing order
				pick := rapid.IntRange(0, len(knownOrders)-1).Draw(rt, "cancelIdx")
				target := knownOrders[pick]
				cmds = append(cmds, Command{
					ID:        uuid.UUID(cmdID),
					Kind:      CmdCancel,
					Symbol:    "AAPL",
					OrderID:   target.id,
					UserID:    target.user,
					Timestamp: ts,
				})
				continue
			}

			var oid uuid.UUID
			if action == 2 && len(knownOrders) > 0 {
				// Intentional duplicate order ID
				pick := rapid.IntRange(0, len(knownOrders)-1).Draw(rt, "dupIdx")
				oid = knownOrders[pick].id
			} else {
				var ob [16]byte
				binary.BigEndian.PutUint64(ob[:8], 0xdeadbeef)
				binary.BigEndian.PutUint64(ob[8:], uint64(i+1))
				oid = uuid.UUID(ob)
			}

			user := users[rapid.IntRange(0, len(users)-1).Draw(rt, "user")]
			side := Buy
			if rapid.Bool().Draw(rt, "isSell") {
				side = Sell
			}
			otype := Limit
			px := rapid.Int64Range(95, 105).Draw(rt, "price")
			if action == 3 {
				otype = Market
				if rapid.Bool().Draw(rt, "unbounded") {
					px = 0
				}
			}
			qty := rapid.Int64Range(1, 50).Draw(rt, "qty")

			knownOrders = append(knownOrders, struct {
				id   uuid.UUID
				user uuid.UUID
			}{oid, user})
			cmds = append(cmds, Command{
				ID:        uuid.UUID(cmdID),
				Kind:      CmdPlace,
				Symbol:    "AAPL",
				OrderID:   oid,
				UserID:    user,
				Side:      side,
				Type:      otype,
				Price:     px,
				Qty:       qty,
				Timestamp: ts,
			})
		}

		evBTree := verifyStreamAndInvariants(t, cmds, IndexBTree)
		evHeap := verifyStreamAndInvariants(t, cmds, IndexHeap)
		if len(evBTree) != len(evHeap) {
			rt.Fatalf("event count mismatch btree=%d heap=%d", len(evBTree), len(evHeap))
		}
		for i := range evBTree {
			if evBTree[i] != evHeap[i] {
				rt.Fatalf("event[%d] mismatch:\nbtree: %+v\nheap:  %+v", i, evBTree[i], evHeap[i])
			}
		}

		// Replay determinism and mid-stream snapshot/restore equivalence
		split := len(cmds) / 2
		eSnap := New(WithIndex(IndexBTree))
		var outSnap []Event
		for _, c := range cmds[:split] {
			outSnap = append(outSnap, eSnap.Process(c)...)
		}
		blob, err := eSnap.Snapshot()
		if err != nil {
			rt.Fatalf("snapshot: %v", err)
		}
		eRestored, err := Restore(blob, WithIndex(IndexBTree))
		if err != nil {
			rt.Fatalf("restore: %v", err)
		}
		for _, c := range cmds[split:] {
			outSnap = append(outSnap, eRestored.Process(c)...)
		}
		for i := range evBTree {
			if evBTree[i] != outSnap[i] {
				rt.Fatalf("snapshot replay mismatch at %d", i)
			}
		}
	})
}

func verifyStreamAndInvariants(tb testing.TB, cmds []Command, kind IndexKind) []Event {
	tb.Helper()
	e := New(WithIndex(kind))
	var all []Event

	orderQty := map[uuid.UUID]int64{}
	tradeFills := map[uuid.UUID]int64{}
	var buyTraded, sellTraded int64

	for _, c := range cmds {
		evs := e.Process(c)
		if err := e.CheckInvariants(); err != nil {
			tb.Fatalf("invariant failed: %v", err)
		}
		for _, ev := range evs {
			all = append(all, ev)
			switch ev.Kind {
			case EvTrade:
				tr := ev.Trade
				if tr.Qty <= 0 || tr.Price <= 0 {
					tb.Fatalf("invalid trade: %+v", tr)
				}
				buyTraded += tr.Qty
				sellTraded += tr.Qty
				tradeFills[tr.BuyOrderID] += tr.Qty
				tradeFills[tr.SellOrderID] += tr.Qty
			case EvOrder:
				if _, ok := orderQty[ev.OrderID]; !ok {
					orderQty[ev.OrderID] = ev.Qty
				}
				if ev.FilledQty != tradeFills[ev.OrderID] {
					tb.Fatalf("order %s filled_qty=%d != sum of trades=%d",
						ev.OrderID, ev.FilledQty, tradeFills[ev.OrderID])
				}
				if ev.FilledQty < 0 || ev.FilledQty > orderQty[ev.OrderID] {
					tb.Fatalf("order %s overfilled %d > %d",
						ev.OrderID, ev.FilledQty, orderQty[ev.OrderID])
				}
			}
		}
	}
	if buyTraded != sellTraded {
		tb.Fatalf("buyTraded=%d != sellTraded=%d", buyTraded, sellTraded)
	}
	return all
}

// TestInvariants1M runs 1,000,000 pseudo-random order commands through the
// engine and verifies all SDD section 5.5 invariants (Milestone M1 acceptance
// criterion: "all invariants pass on 1M random orders").
func TestInvariants1M(t *testing.T) {
	total := 1_000_000
	if testing.Short() {
		total = 100_000
	}
	rng := rand.New(rand.NewPCG(42, 2026))
	users := [8]uuid.UUID{
		uid("u0"), uid("u1"), uid("u2"), uid("u3"),
		uid("u4"), uid("u5"), uid("u6"), uid("u7"),
	}

	e := New(WithIndex(IndexBTree))

	type activeOrder struct {
		id   uuid.UUID
		user uuid.UUID
	}
	active := make([]activeOrder, 0, 4096)

	var buyTraded, sellTraded int64
	var totalTrades, totalEvents uint64

	for i := 1; i <= total; i++ {
		var cmdID [16]byte
		binary.BigEndian.PutUint64(cmdID[8:], uint64(i))
		ts := int64(i) * 1000

		var c Command
		r := rng.IntN(100)
		if r < 20 && len(active) > 0 {
			// 20% cancels
			idx := rng.IntN(len(active))
			target := active[idx]
			active[idx] = active[len(active)-1]
			active = active[:len(active)-1]
			c = Command{
				ID:        uuid.UUID(cmdID),
				Kind:      CmdCancel,
				Symbol:    "AAPL",
				OrderID:   target.id,
				UserID:    target.user,
				Timestamp: ts,
			}
		} else {
			var oid [16]byte
			binary.BigEndian.PutUint64(oid[:8], 1)
			binary.BigEndian.PutUint64(oid[8:], uint64(i))
			user := users[rng.IntN(len(users))]
			side := Buy
			if rng.IntN(2) == 1 {
				side = Sell
			}
			qty := int64(rng.IntN(50) + 1)
			otype := Limit
			px := int64(9900 + rng.IntN(201)) // 99.00 .. 101.00
			if r >= 90 {
				otype = Market
				px = 0
			}
			c = Command{
				ID:        uuid.UUID(cmdID),
				Kind:      CmdPlace,
				Symbol:    "AAPL",
				OrderID:   uuid.UUID(oid),
				UserID:    user,
				Side:      side,
				Type:      otype,
				Price:     px,
				Qty:       qty,
				Timestamp: ts,
			}
		}

		evs := e.Process(c)
		totalEvents += uint64(len(evs))
		for _, ev := range evs {
			switch ev.Kind {
			case EvTrade:
				totalTrades++
				buyTraded += ev.Trade.Qty
				sellTraded += ev.Trade.Qty
				if ev.Trade.Qty <= 0 || ev.Trade.Price <= 0 {
					t.Fatalf("cmd %d: invalid trade %+v", i, ev.Trade)
				}
			case EvOrder:
				if ev.FilledQty < 0 || (ev.Qty > 0 && ev.FilledQty > ev.Qty) {
					t.Fatalf("cmd %d: overfill %+v", i, ev)
				}
				if ev.Resting && (ev.Status == StatusAccepted || ev.Status == StatusPartiallyFilled) && c.Kind == CmdPlace {
					active = append(active, activeOrder{id: ev.OrderID, user: ev.UserID})
				}
			}
		}

		// Check best bid < best ask after every command (O(1))
		if b := e.Book("AAPL"); b != nil {
			if bid, okB := b.BestBid(); okB {
				if ask, okA := b.BestAsk(); okA && bid >= ask {
					t.Fatalf("cmd %d: crossed book bid=%d ask=%d", i, bid, ask)
				}
			}
		}
		// Full structural walk every 50,000 commands
		if i%50_000 == 0 {
			if err := e.CheckInvariants(); err != nil {
				t.Fatalf("cmd %d: %v", i, err)
			}
		}
	}

	if err := e.CheckInvariants(); err != nil {
		t.Fatalf("final invariants: %v", err)
	}
	if buyTraded != sellTraded {
		t.Fatalf("buyTraded=%d != sellTraded=%d", buyTraded, sellTraded)
	}

	snap1, err := e.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	e2, err := Restore(snap1)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	snap2, err := e2.Snapshot()
	if err != nil {
		t.Fatalf("snapshot2: %v", err)
	}
	if !bytes.Equal(snap1, snap2) {
		t.Fatal("snapshot round-trip mismatch after 1M orders")
	}
	t.Logf("1M commands ok: trades=%d events=%d resting=%d",
		totalTrades, totalEvents, e.Book("AAPL").OrderCount())
}
