package engine

import (
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"pgregory.net/rapid"
)

// ---- test helpers -----------------------------------------------------------

var (
	alice = uid("alice")
	bob   = uid("bob")
	carol = uid("carol")
)

func uid(s string) uuid.UUID { return uuid.NewSHA1(uuid.NameSpaceOID, []byte(s)) }

type harness struct {
	t   *testing.T
	e   *Engine
	n   int
	all []Event
}

func newHarness(t *testing.T, opts ...Option) *harness {
	return &harness{t: t, e: New(opts...)}
}

func (h *harness) next() (uuid.UUID, int64) {
	h.n++
	return uid(fmt.Sprintf("cmd-%d", h.n)), int64(h.n) * 1000
}

func (h *harness) do(c Command) []Event {
	h.t.Helper()
	c.ID, c.Timestamp = h.next()
	if c.Symbol == "" && c.Kind != 0 {
		c.Symbol = "AAPL"
	}
	evs := append([]Event(nil), h.e.Process(c)...)
	h.all = append(h.all, evs...)
	if err := h.e.CheckInvariants(); err != nil {
		h.t.Fatalf("invariants after %+v: %v", c, err)
	}
	return evs
}

func (h *harness) limit(user uuid.UUID, oid string, side Side, px, qty int64) []Event {
	return h.do(Command{Kind: CmdPlace, OrderID: uid(oid), UserID: user, Side: side, Type: Limit, Price: px, Qty: qty})
}

func (h *harness) market(user uuid.UUID, oid string, side Side, qty, protect int64) []Event {
	return h.do(Command{Kind: CmdPlace, OrderID: uid(oid), UserID: user, Side: side, Type: Market, Price: protect, Qty: qty})
}

func (h *harness) cancel(user uuid.UUID, oid string) []Event {
	return h.do(Command{Kind: CmdCancel, OrderID: uid(oid), UserID: user})
}

func (h *harness) replace(user uuid.UUID, oid string, px, qty int64) []Event {
	return h.do(Command{Kind: CmdReplace, OrderID: uid(oid), UserID: user, Price: px, Qty: qty})
}

// kindOf returns the single event of the given kind in evs, failing if there is
// not exactly one.
func kindOf(t *testing.T, evs []Event, k EventKind) Event {
	t.Helper()
	var out []Event
	for _, e := range evs {
		if e.Kind == k {
			out = append(out, e)
		}
	}
	if len(out) != 1 {
		t.Fatalf("want exactly one event of kind %d, got %d in %+v", k, len(out), evs)
	}
	return out[0]
}

func trades(evs []Event) []Trade {
	var out []Trade
	for _, e := range evs {
		if e.Kind == EvTrade {
			out = append(out, e.Trade)
		}
	}
	return out
}

// last returns the final execution report for oid within evs.
func last(t *testing.T, evs []Event, oid string) Event {
	t.Helper()
	id := uid(oid)
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Kind == EvOrder && evs[i].OrderID == id {
			return evs[i]
		}
	}
	t.Fatalf("no order event for %s", oid)
	return Event{}
}

func depth(h *harness) (bids, asks []Level) {
	b := h.e.Book("AAPL")
	if b == nil {
		return nil, nil
	}
	return b.Depth(0)
}

// ---- scenarios ------------------------------------------------------------------

func TestRestingLimitOrders(t *testing.T) {
	for _, kind := range []IndexKind{IndexBTree, IndexHeap} {
		t.Run(fmt.Sprint(kind), func(t *testing.T) {
			h := newHarness(t, WithIndex(kind))
			ev := h.limit(alice, "b1", Buy, 100, 10)
			if got := last(t, ev, "b1"); got.Status != StatusAccepted || !got.Resting || got.LeavesQty != 10 {
				t.Fatalf("unexpected %+v", got)
			}
			h.limit(alice, "b2", Buy, 101, 5)
			h.limit(alice, "b3", Buy, 100, 7)
			h.limit(bob, "a1", Sell, 103, 4)
			h.limit(bob, "a2", Sell, 102, 6)

			bids, asks := depth(h)
			wantB := []Level{{101, 5, 1}, {100, 17, 2}}
			wantA := []Level{{102, 6, 1}, {103, 4, 1}}
			if fmt.Sprint(bids) != fmt.Sprint(wantB) || fmt.Sprint(asks) != fmt.Sprint(wantA) {
				t.Fatalf("depth bids=%v asks=%v", bids, asks)
			}
		})
	}
}

func TestFullFillAtRestingPrice(t *testing.T) {
	h := newHarness(t)
	h.limit(alice, "a1", Sell, 100, 10)
	ev := h.limit(bob, "b1", Buy, 105, 10) // aggressive buy, price improvement

	tr := trades(ev)
	if len(tr) != 1 || tr[0].Price != 100 || tr[0].Qty != 10 {
		t.Fatalf("trades %+v", tr)
	}
	if tr[0].BuyerID != bob || tr[0].SellerID != alice || tr[0].Aggressor != Buy || tr[0].MakerOrder != uid("a1") {
		t.Fatalf("trade parties %+v", tr[0])
	}
	if s := last(t, ev, "a1"); s.Status != StatusFilled || s.Resting {
		t.Fatalf("maker %+v", s)
	}
	if s := last(t, ev, "b1"); s.Status != StatusFilled || s.FilledQty != 10 {
		t.Fatalf("taker %+v", s)
	}
	if b, a := depth(h); len(b)+len(a) != 0 {
		t.Fatal("book should be empty")
	}
}

func TestPartialFillTakerRests(t *testing.T) {
	h := newHarness(t)
	h.limit(alice, "a1", Sell, 100, 4)
	ev := h.limit(bob, "b1", Buy, 100, 10)
	if s := last(t, ev, "b1"); s.Status != StatusPartiallyFilled || s.FilledQty != 4 || s.LeavesQty != 6 || !s.Resting {
		t.Fatalf("taker %+v", s)
	}
	bids, asks := depth(h)
	if len(asks) != 0 || len(bids) != 1 || bids[0] != (Level{100, 6, 1}) {
		t.Fatalf("depth %v %v", bids, asks)
	}
}

func TestPartialFillMakerStays(t *testing.T) {
	h := newHarness(t)
	h.limit(alice, "a1", Sell, 100, 10)
	ev := h.limit(bob, "b1", Buy, 100, 3)
	if s := last(t, ev, "a1"); s.Status != StatusPartiallyFilled || s.LeavesQty != 7 || !s.Resting {
		t.Fatalf("maker %+v", s)
	}
	_, asks := depth(h)
	if asks[0] != (Level{100, 7, 1}) {
		t.Fatalf("asks %v", asks)
	}
}

func TestMultiLevelSweep(t *testing.T) {
	h := newHarness(t)
	h.limit(alice, "a1", Sell, 100, 2)
	h.limit(carol, "a2", Sell, 100, 3)
	h.limit(alice, "a3", Sell, 101, 4)
	h.limit(carol, "a4", Sell, 103, 5)
	ev := h.limit(bob, "b1", Buy, 102, 20)

	tr := trades(ev)
	want := []struct{ px, qty int64 }{{100, 2}, {100, 3}, {101, 4}}
	if len(tr) != len(want) {
		t.Fatalf("trades %+v", tr)
	}
	for i, w := range want {
		if tr[i].Price != w.px || tr[i].Qty != w.qty {
			t.Fatalf("trade %d = %+v want %+v", i, tr[i], w)
		}
	}
	if tr[0].MakerOrder != uid("a1") || tr[1].MakerOrder != uid("a2") {
		t.Fatal("time priority violated")
	}
	if s := last(t, ev, "b1"); s.Status != StatusPartiallyFilled || s.FilledQty != 9 || s.LeavesQty != 11 {
		t.Fatalf("taker %+v", s)
	}
	bids, asks := depth(h)
	if bids[0] != (Level{102, 11, 1}) || asks[0] != (Level{103, 5, 1}) {
		t.Fatalf("depth %v %v", bids, asks)
	}
}

func TestTimePriorityWithinLevel(t *testing.T) {
	h := newHarness(t)
	h.limit(alice, "b1", Buy, 100, 5)
	h.limit(carol, "b2", Buy, 100, 5)
	ev := h.limit(bob, "s1", Sell, 100, 7)
	tr := trades(ev)
	if len(tr) != 2 || tr[0].BuyOrderID != uid("b1") || tr[0].Qty != 5 || tr[1].BuyOrderID != uid("b2") || tr[1].Qty != 2 {
		t.Fatalf("trades %+v", tr)
	}
}

func TestSellSideSweepPricesDescending(t *testing.T) {
	h := newHarness(t)
	h.limit(alice, "b1", Buy, 99, 1)
	h.limit(alice, "b2", Buy, 101, 1)
	h.limit(alice, "b3", Buy, 100, 1)
	tr := trades(h.limit(bob, "s1", Sell, 98, 3))
	if len(tr) != 3 || tr[0].Price != 101 || tr[1].Price != 100 || tr[2].Price != 99 {
		t.Fatalf("trades %+v", tr)
	}
}

func TestCancel(t *testing.T) {
	h := newHarness(t)
	h.limit(alice, "b1", Buy, 100, 10)
	h.limit(bob, "s1", Sell, 100, 4)
	ev := h.cancel(alice, "b1")
	if len(ev) != 1 {
		t.Fatalf("events %+v", ev)
	}
	s := ev[0]
	if s.Status != StatusCancelled || s.Reason != ReasonUserCancel || s.FilledQty != 4 || s.LeavesQty != 0 || s.Resting {
		t.Fatalf("cancel %+v", s)
	}
	if b, _ := depth(h); len(b) != 0 {
		t.Fatal("bid should be gone")
	}
	// second cancel is rejected
	if ev := h.cancel(alice, "b1"); ev[0].Kind != EvCancelReject || ev[0].Reason != ReasonUnknownOrder {
		t.Fatalf("second cancel %+v", ev)
	}
}

func TestCancelByOtherUserRejected(t *testing.T) {
	h := newHarness(t)
	h.limit(alice, "b1", Buy, 100, 10)
	if ev := h.cancel(bob, "b1"); ev[0].Kind != EvCancelReject {
		t.Fatalf("got %+v", ev)
	}
	if b, _ := depth(h); len(b) != 1 {
		t.Fatal("order must remain")
	}
}

func TestCancelMiddleOfLevelKeepsFIFO(t *testing.T) {
	h := newHarness(t)
	h.limit(alice, "b1", Buy, 100, 1)
	h.limit(alice, "b2", Buy, 100, 1)
	h.limit(alice, "b3", Buy, 100, 1)
	h.cancel(alice, "b2")
	tr := trades(h.limit(bob, "s1", Sell, 100, 2))
	if tr[0].BuyOrderID != uid("b1") || tr[1].BuyOrderID != uid("b3") {
		t.Fatalf("fifo broken %+v", tr)
	}
}

func TestMarketOrderFullFill(t *testing.T) {
	h := newHarness(t)
	h.limit(alice, "a1", Sell, 100, 5)
	h.limit(alice, "a2", Sell, 110, 5)
	ev := h.market(bob, "m1", Buy, 8, 0)
	tr := trades(ev)
	if len(tr) != 2 || tr[1].Price != 110 || tr[1].Qty != 3 {
		t.Fatalf("trades %+v", tr)
	}
	if s := last(t, ev, "m1"); s.Status != StatusFilled || s.Resting {
		t.Fatalf("market %+v", s)
	}
}

func TestMarketOrderThinBookCancelsRemainder(t *testing.T) {
	h := newHarness(t)
	h.limit(alice, "a1", Sell, 100, 3)
	ev := h.market(bob, "m1", Buy, 10, 0)
	s := last(t, ev, "m1")
	if s.Status != StatusCancelled || s.Reason != ReasonIOCRemainder || s.FilledQty != 3 || s.LeavesQty != 0 || s.Resting {
		t.Fatalf("market %+v", s)
	}
	if b, a := depth(h); len(b)+len(a) != 0 {
		t.Fatal("market orders must never rest")
	}
}

func TestMarketOrderEmptyBookRejected(t *testing.T) {
	h := newHarness(t)
	ev := h.market(bob, "m1", Sell, 10, 0)
	if s := last(t, ev, "m1"); s.Status != StatusRejected || s.Reason != ReasonNoLiquidity {
		t.Fatalf("market %+v", s)
	}
}

func TestMarketOrderProtectionPrice(t *testing.T) {
	h := newHarness(t)
	h.limit(alice, "a1", Sell, 100, 2)
	h.limit(alice, "a2", Sell, 106, 5)
	ev := h.market(bob, "m1", Buy, 5, 105) // band: never above 105
	tr := trades(ev)
	if len(tr) != 1 || tr[0].Price != 100 {
		t.Fatalf("trades %+v", tr)
	}
	if s := last(t, ev, "m1"); s.Status != StatusCancelled || s.Reason != ReasonPriceBand || s.FilledQty != 2 {
		t.Fatalf("market %+v", s)
	}
	// Entirely outside the band: rejected with PRICE_BAND.
	ev = h.market(bob, "m2", Buy, 1, 105)
	if s := last(t, ev, "m2"); s.Status != StatusRejected || s.Reason != ReasonPriceBand {
		t.Fatalf("market %+v", s)
	}
}

func TestSelfTradePreventionCancelsNewest(t *testing.T) {
	h := newHarness(t)
	h.limit(carol, "a0", Sell, 99, 1)
	h.limit(alice, "a1", Sell, 100, 5)
	ev := h.limit(alice, "b1", Buy, 100, 5)
	tr := trades(ev)
	if len(tr) != 0 {
		// alice's buy would first hit carol's 99 ask: that is fine
		if len(tr) != 1 || tr[0].SellerID != carol {
			t.Fatalf("trades %+v", tr)
		}
	}
	s := last(t, ev, "b1")
	if s.Status != StatusCancelled || s.Reason != ReasonSelfTrade || s.FilledQty != 1 || s.Resting {
		t.Fatalf("stp %+v", s)
	}
	_, asks := depth(h)
	if len(asks) != 1 || asks[0] != (Level{100, 5, 1}) {
		t.Fatalf("resting maker must survive STP: %v", asks)
	}
}

func TestDuplicatePlaceIgnored(t *testing.T) {
	h := newHarness(t)
	h.limit(alice, "a1", Sell, 100, 5)
	h.limit(bob, "b1", Buy, 100, 5) // fully fills a1, removing it from the book
	ev := h.limit(alice, "a1", Sell, 100, 5)
	if len(ev) != 1 || ev[0].Kind != EvDuplicate {
		t.Fatalf("duplicate must be ignored: %+v", ev)
	}
	if b, a := depth(h); len(b)+len(a) != 0 {
		t.Fatal("duplicate must not rest")
	}
}

func TestDedupeWindowExpires(t *testing.T) {
	h := newHarness(t, WithDedupeWindow(5000)) // 5 commands worth of time
	h.market(alice, "m1", Buy, 1, 0)           // rejected, but id remembered
	for i := 0; i < 10; i++ {
		h.limit(bob, fmt.Sprintf("x%d", i), Buy, 1, 1)
	}
	if h.e.seen.ids[uid("m1")] == struct{}{} {
		if _, ok := h.e.seen.ids[uid("m1")]; ok {
			t.Fatal("m1 should have expired from the dedupe window")
		}
	}
}

func TestInvalidCommandsRejected(t *testing.T) {
	cases := map[string]Command{
		"zero qty":       {Kind: CmdPlace, OrderID: uid("o"), Side: Buy, Type: Limit, Price: 100, Qty: 0},
		"negative price": {Kind: CmdPlace, OrderID: uid("o"), Side: Buy, Type: Limit, Price: -1, Qty: 1},
		"limit no price": {Kind: CmdPlace, OrderID: uid("o"), Side: Buy, Type: Limit, Qty: 1},
		"bad side":       {Kind: CmdPlace, OrderID: uid("o"), Side: 9, Type: Limit, Price: 1, Qty: 1},
		"bad type":       {Kind: CmdPlace, OrderID: uid("o"), Side: Buy, Type: 9, Price: 1, Qty: 1},
		"nil order id":   {Kind: CmdPlace, Side: Buy, Type: Limit, Price: 1, Qty: 1},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			ev := h.do(c)
			if len(ev) != 1 || ev[0].Status != StatusRejected || ev[0].Reason != ReasonInvalid {
				t.Fatalf("got %+v", ev)
			}
		})
	}
}

func TestSequenceNumbersGapFreePerSymbol(t *testing.T) {
	h := newHarness(t)
	h.limit(alice, "a1", Sell, 100, 5)
	h.do(Command{Kind: CmdPlace, Symbol: "MSFT", OrderID: uid("m"), UserID: alice, Side: Buy, Type: Limit, Price: 1, Qty: 1})
	h.limit(bob, "b1", Buy, 100, 2)
	h.cancel(alice, "a1")
	seqs := map[string]uint64{}
	for _, ev := range h.all {
		if ev.Seq != seqs[ev.Symbol]+1 {
			t.Fatalf("%s gap: got %d after %d", ev.Symbol, ev.Seq, seqs[ev.Symbol])
		}
		seqs[ev.Symbol] = ev.Seq
	}
}

func TestEventIDsDeterministicAndUnique(t *testing.T) {
	run := func() []Event {
		h := newHarness(t)
		h.limit(alice, "a1", Sell, 100, 5)
		h.limit(bob, "b1", Buy, 101, 8)
		h.cancel(bob, "b1")
		return h.all
	}
	a, b := run(), run()
	seen := map[uuid.UUID]bool{}
	for i := range a {
		if a[i].ID != b[i].ID {
			t.Fatal("non-deterministic event id")
		}
		if seen[a[i].ID] {
			t.Fatal("duplicate event id")
		}
		seen[a[i].ID] = true
	}
}

func TestSnapshotRestorePreservesPriority(t *testing.T) {
	for _, kind := range []IndexKind{IndexBTree, IndexHeap} {
		h := newHarness(t, WithIndex(kind))
		h.limit(alice, "b1", Buy, 100, 1)
		h.limit(carol, "b2", Buy, 100, 1)
		h.limit(alice, "b3", Buy, 99, 4)
		h.limit(carol, "a1", Sell, 105, 2)
		h.limit(bob, "s1", Sell, 100, 0+1) // partially consumes level 100
		snap, err := h.e.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		e2, err := Restore(snap, WithIndex(kind))
		if err != nil {
			t.Fatal(err)
		}
		snap2, _ := e2.Snapshot()
		if string(snap) != string(snap2) {
			t.Fatal("snapshot not stable across restore")
		}
		if e2.Book("AAPL").Seq() != h.e.Book("AAPL").Seq() {
			t.Fatal("seq not restored")
		}
		// Next sell must hit b2 (b1 already filled), proving FIFO survived.
		h2 := &harness{t: t, e: e2, n: h.n}
		tr := trades(h2.limit(bob, "s2", Sell, 99, 1))
		if len(tr) != 1 || tr[0].BuyOrderID != uid("b2") {
			t.Fatalf("priority lost: %+v", tr)
		}
		// Duplicate detection survives restore too.
		if ev := h2.limit(alice, "b1", Buy, 100, 1); ev[0].Kind != EvDuplicate {
			t.Fatal("dedupe state lost")
		}
	}
}

// ---- order replace (cancel-replace) ------------------------------------------

func TestReplaceReduceQtyKeepsTimePriority(t *testing.T) {
	for _, kind := range []IndexKind{IndexBTree, IndexHeap} {
		t.Run(fmt.Sprint(kind), func(t *testing.T) {
			h := newHarness(t, WithIndex(kind))
			h.limit(alice, "b1", Buy, 100, 10) // first in the queue
			h.limit(carol, "b2", Buy, 100, 5)

			ev := h.replace(alice, "b1", 100, 4) // same price, smaller: in place
			r := kindOf(t, ev, EvReplaced)
			if r.OrderID != uid("b1") || r.Price != 100 || r.Qty != 4 || r.LeavesQty != 4 || !r.Resting ||
				r.Status != StatusAccepted || r.PrevPrice != 100 || r.PrevQty != 10 || r.PrevFilled != 0 {
				t.Fatalf("replaced %+v", r)
			}
			if len(ev) != 1 {
				t.Fatalf("a pure reduce emits only the replaced report: %+v", ev)
			}
			if bids, _ := depth(h); bids[0] != (Level{100, 9, 2}) {
				t.Fatalf("depth %v", bids)
			}

			// b1 kept its place: it trades first even though b2 is not newer.
			tr := trades(h.limit(bob, "s1", Sell, 100, 6))
			if len(tr) != 2 || tr[0].BuyOrderID != uid("b1") || tr[0].Qty != 4 ||
				tr[1].BuyOrderID != uid("b2") || tr[1].Qty != 2 {
				t.Fatalf("time priority lost on qty reduction: %+v", tr)
			}
		})
	}
}

func TestReplaceIncreaseQtyLosesTimePriority(t *testing.T) {
	h := newHarness(t)
	h.limit(alice, "b1", Buy, 100, 5)
	h.limit(carol, "b2", Buy, 100, 5)

	ev := h.replace(alice, "b1", 100, 8)
	if r := kindOf(t, ev, EvReplaced); r.Qty != 8 || r.LeavesQty != 8 || r.PrevQty != 5 || !r.Resting {
		t.Fatalf("replaced %+v", r)
	}
	if bids, _ := depth(h); bids[0] != (Level{100, 13, 2}) {
		t.Fatalf("depth %v", bids)
	}

	// b2 is now ahead of b1.
	tr := trades(h.limit(bob, "s1", Sell, 100, 6))
	if len(tr) != 2 || tr[0].BuyOrderID != uid("b2") || tr[0].Qty != 5 ||
		tr[1].BuyOrderID != uid("b1") || tr[1].Qty != 1 {
		t.Fatalf("increasing qty must forfeit priority: %+v", tr)
	}
}

func TestReplacePriceChangeLosesTimePriority(t *testing.T) {
	h := newHarness(t)
	h.limit(alice, "b1", Buy, 99, 5)
	h.limit(carol, "b2", Buy, 100, 5)

	// b1 moves up to 100 and queues behind b2, even though it is the older order.
	ev := h.replace(alice, "b1", 100, 5)
	if r := kindOf(t, ev, EvReplaced); r.Price != 100 || r.PrevPrice != 99 || r.Qty != 5 || !r.Resting {
		t.Fatalf("replaced %+v", r)
	}
	bids, _ := depth(h)
	if len(bids) != 1 || bids[0] != (Level{100, 10, 2}) {
		t.Fatalf("level 99 must be gone, depth %v", bids)
	}
	tr := trades(h.limit(bob, "s1", Sell, 100, 6))
	if len(tr) != 2 || tr[0].BuyOrderID != uid("b2") || tr[1].BuyOrderID != uid("b1") {
		t.Fatalf("price change must forfeit priority: %+v", tr)
	}
}

func TestReplacePriceAndQtyTogether(t *testing.T) {
	h := newHarness(t)
	h.limit(alice, "a1", Sell, 105, 10)
	ev := h.replace(alice, "a1", 103, 7)
	r := kindOf(t, ev, EvReplaced)
	if r.Price != 103 || r.Qty != 7 || r.PrevPrice != 105 || r.PrevQty != 10 || r.LeavesQty != 7 {
		t.Fatalf("replaced %+v", r)
	}
	if _, asks := depth(h); len(asks) != 1 || asks[0] != (Level{103, 7, 1}) {
		t.Fatalf("depth %v", asks)
	}
}

func TestReplaceCrossingPriceTradesAsTaker(t *testing.T) {
	h := newHarness(t)
	h.limit(carol, "a1", Sell, 100, 4)
	h.limit(carol, "a2", Sell, 101, 4)
	h.limit(alice, "b1", Buy, 95, 10)

	ev := h.replace(alice, "b1", 101, 10) // now crosses both asks
	tr := trades(ev)
	if len(tr) != 2 || tr[0].Price != 100 || tr[0].Qty != 4 || tr[1].Price != 101 || tr[1].Qty != 4 {
		t.Fatalf("trades %+v", tr)
	}
	for _, x := range tr {
		if x.BuyOrderID != uid("b1") || x.Aggressor != Buy || x.BuyLimit != 101 {
			t.Fatalf("trade must carry the replaced order's new limit: %+v", x)
		}
	}
	// The replaced report is last and reflects the post-trade state.
	r := ev[len(ev)-1]
	if r.Kind != EvReplaced || r.FilledQty != 8 || r.LeavesQty != 2 || r.Status != StatusPartiallyFilled ||
		!r.Resting || r.PrevFilled != 0 || r.PrevPrice != 95 {
		t.Fatalf("replaced %+v", r)
	}
	bids, asks := depth(h)
	if len(asks) != 0 || len(bids) != 1 || bids[0] != (Level{101, 2, 1}) {
		t.Fatalf("depth %v %v", bids, asks)
	}
}

func TestReplaceCrossingPriceFullFill(t *testing.T) {
	h := newHarness(t)
	h.limit(bob, "a1", Sell, 100, 5)
	h.limit(alice, "b1", Buy, 95, 3)
	ev := h.replace(alice, "b1", 100, 3)
	r := ev[len(ev)-1]
	if r.Kind != EvReplaced || r.Status != StatusFilled || r.FilledQty != 3 || r.LeavesQty != 0 || r.Resting {
		t.Fatalf("replaced %+v", r)
	}
	if b, a := depth(h); len(b) != 0 || len(a) != 1 || a[0] != (Level{100, 2, 1}) {
		t.Fatalf("depth %v %v", b, a)
	}
	// It is gone from the book, so it cannot be replaced or cancelled again.
	if ev := h.replace(alice, "b1", 99, 3); ev[0].Kind != EvReplaceReject || ev[0].Reason != ReasonUnknownOrder {
		t.Fatalf("got %+v", ev)
	}
}

func TestReplaceSelfTradePreventionCancelsReplacedOrder(t *testing.T) {
	h := newHarness(t)
	h.limit(alice, "a1", Sell, 100, 5)
	h.limit(alice, "b1", Buy, 95, 5)
	ev := h.replace(alice, "b1", 100, 5) // would trade against her own ask
	r := kindOf(t, ev, EvReplaced)
	if r.Status != StatusCancelled || r.Reason != ReasonSelfTrade || r.Resting || r.LeavesQty != 0 || len(trades(ev)) != 0 {
		t.Fatalf("replaced %+v", r)
	}
	bids, asks := depth(h)
	if len(bids) != 0 || len(asks) != 1 || asks[0] != (Level{100, 5, 1}) {
		t.Fatalf("resting maker must survive, the replaced order must be gone: %v %v", bids, asks)
	}
}

func TestReplaceAfterPartialFillIsRelativeToTotalQty(t *testing.T) {
	h := newHarness(t)
	h.limit(alice, "b1", Buy, 100, 10)
	h.limit(bob, "s1", Sell, 100, 4) // b1 now filled 4, leaves 6

	ev := h.replace(alice, "b1", 100, 6) // new TOTAL qty 6 -> leaves 2
	r := kindOf(t, ev, EvReplaced)
	if r.Qty != 6 || r.FilledQty != 4 || r.LeavesQty != 2 || r.PrevQty != 10 || r.PrevFilled != 4 ||
		r.Status != StatusPartiallyFilled {
		t.Fatalf("replaced %+v", r)
	}
	if bids, _ := depth(h); bids[0] != (Level{100, 2, 1}) {
		t.Fatalf("depth %v", bids)
	}
	tr := trades(h.limit(bob, "s2", Sell, 100, 5))
	if len(tr) != 1 || tr[0].Qty != 2 {
		t.Fatalf("only the 2 remaining shares may trade: %+v", tr)
	}
	if s := last(t, h.all, "b1"); s.Status != StatusFilled || s.FilledQty != 6 || s.Qty != 6 {
		t.Fatalf("order ends filled at its new total: %+v", s)
	}
}

func TestReplaceRejected(t *testing.T) {
	h := newHarness(t)
	h.limit(alice, "b1", Buy, 100, 10)
	h.limit(bob, "s1", Sell, 100, 4) // b1: filled 4, leaves 6

	cases := []struct {
		name   string
		cmd    Command
		reason Reason
	}{
		{"unknown order", Command{OrderID: uid("nope"), UserID: alice, Price: 100, Qty: 5}, ReasonUnknownOrder},
		{"someone else's order", Command{OrderID: uid("b1"), UserID: bob, Price: 99, Qty: 10}, ReasonUnknownOrder},
		{"qty equal to filled", Command{OrderID: uid("b1"), UserID: alice, Price: 100, Qty: 4}, ReasonQtyTooLow},
		{"qty below filled", Command{OrderID: uid("b1"), UserID: alice, Price: 99, Qty: 3}, ReasonQtyTooLow},
		{"nothing changes", Command{OrderID: uid("b1"), UserID: alice, Price: 100, Qty: 10}, ReasonNoChange},
		{"zero price", Command{OrderID: uid("b1"), UserID: alice, Price: 0, Qty: 10}, ReasonInvalid},
		{"negative qty", Command{OrderID: uid("b1"), UserID: alice, Price: 100, Qty: -1}, ReasonInvalid},
		{"nil order id", Command{UserID: alice, Price: 100, Qty: 10}, ReasonInvalid},
	}
	before, _ := h.e.Snapshot()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.cmd.Kind = CmdReplace
			ev := h.do(c.cmd)
			if len(ev) != 1 || ev[0].Kind != EvReplaceReject || ev[0].Reason != c.reason {
				t.Fatalf("got %+v", ev)
			}
		})
	}
	// None of the rejections may have touched the book. (Snapshots also hold
	// the dedupe window, which does remember the rejected command ids, so
	// compare the book itself.)
	after, _ := h.e.Snapshot()
	if bids, _ := depth(h); len(bids) != 1 || bids[0] != (Level{100, 6, 1}) {
		t.Fatalf("book changed by rejected replaces: %v (snapshots %d/%d bytes)", bids, len(before), len(after))
	}
}

func TestReplaceDuplicateCommandIgnored(t *testing.T) {
	h := newHarness(t)
	h.limit(alice, "b1", Buy, 100, 10)
	cmd := Command{ID: uid("replace-1"), Kind: CmdReplace, Symbol: "AAPL", OrderID: uid("b1"),
		UserID: alice, Price: 100, Qty: 6, Timestamp: 99_000}
	if ev := h.e.Process(cmd); len(ev) != 1 || ev[0].Kind != EvReplaced {
		t.Fatalf("first delivery %+v", ev)
	}
	// An outbox re-publish delivers the same command again.
	if ev := h.e.Process(cmd); len(ev) != 1 || ev[0].Kind != EvDuplicate {
		t.Fatalf("redelivery must be ignored: %+v", ev)
	}
	if bids, _ := depth(h); bids[0] != (Level{100, 6, 1}) {
		t.Fatalf("depth %v", bids)
	}
}

func TestReplaceSnapshotRestorePreservesPriority(t *testing.T) {
	for _, kind := range []IndexKind{IndexBTree, IndexHeap} {
		h := newHarness(t, WithIndex(kind))
		h.limit(alice, "b1", Buy, 100, 9)
		h.limit(carol, "b2", Buy, 100, 5)
		h.limit(alice, "b3", Buy, 100, 5)
		h.replace(alice, "b1", 100, 3) // shrink: keeps the head of the queue
		h.replace(carol, "b2", 100, 6) // grow: moves behind b3
		snap, err := h.e.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		e2, err := Restore(snap, WithIndex(kind))
		if err != nil {
			t.Fatal(err)
		}
		h2 := &harness{t: t, e: e2, n: h.n}
		tr := trades(h2.limit(bob, "s1", Sell, 100, 14))
		want := []struct {
			id  string
			qty int64
		}{{"b1", 3}, {"b3", 5}, {"b2", 6}}
		if len(tr) != len(want) {
			t.Fatalf("trades %+v", tr)
		}
		for i, w := range want {
			if tr[i].BuyOrderID != uid(w.id) || tr[i].Qty != w.qty {
				t.Fatalf("trade %d = %+v, want %s x%d", i, tr[i], w.id, w.qty)
			}
		}
	}
}

func TestReplaceEventsKeepSequenceGapFreeAndDeterministic(t *testing.T) {
	run := func() []Event {
		h := newHarness(t)
		h.limit(alice, "b1", Buy, 95, 10)
		h.limit(carol, "a1", Sell, 100, 4)
		h.replace(alice, "b1", 100, 10) // crossing replace: trade + replaced
		h.replace(alice, "b1", 100, 10) // no change -> reject
		h.replace(alice, "b1", 99, 8)
		h.cancel(alice, "b1")
		return h.all
	}
	a, b := run(), run()
	seq := uint64(0)
	seen := map[uuid.UUID]bool{}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("event %d is not deterministic:\n%+v\n%+v", i, a[i], b[i])
		}
		if a[i].Seq != seq+1 {
			t.Fatalf("seq gap at event %d: %d after %d", i, a[i].Seq, seq)
		}
		seq = a[i].Seq
		if seen[a[i].ID] {
			t.Fatalf("duplicate event id %s", a[i].ID)
		}
		seen[a[i].ID] = true
	}
}

// ---- replace: model-based property test ---------------------------------------

// bookModel is an independent, deliberately naive reconstruction of the book
// from the engine's event stream. It encodes the time-priority rules of
// cancel-replace in the simplest possible way (a FIFO slice per price level),
// and the property test requires the real book to match it after every
// command, and every trade to hit the head of the model's queue.
type bookModel struct {
	orders map[uuid.UUID]*modelOrder
	queues map[modelKey][]uuid.UUID
}

type modelKey struct {
	side  Side
	price int64
}

type modelOrder struct {
	side               Side
	price, qty, filled int64
}

func newBookModel() *bookModel {
	return &bookModel{orders: map[uuid.UUID]*modelOrder{}, queues: map[modelKey][]uuid.UUID{}}
}

func (m *bookModel) push(id uuid.UUID) {
	o := m.orders[id]
	k := modelKey{o.side, o.price}
	m.queues[k] = append(m.queues[k], id)
}

func (m *bookModel) unqueue(id uuid.UUID) {
	o := m.orders[id]
	k := modelKey{o.side, o.price}
	q := m.queues[k]
	for i, x := range q {
		if x == id {
			m.queues[k] = append(q[:i:i], q[i+1:]...)
			break
		}
	}
	if len(m.queues[k]) == 0 {
		delete(m.queues, k)
	}
}

func (m *bookModel) apply(tb testing.TB, evs []Event) {
	tb.Helper()
	for _, ev := range evs {
		switch ev.Kind {
		case EvTrade:
			tr := ev.Trade
			mk := m.orders[tr.MakerOrder]
			if mk == nil {
				tb.Fatalf("trade against a maker the model does not know: %+v", tr)
			}
			if q := m.queues[modelKey{mk.side, mk.price}]; len(q) == 0 || q[0] != tr.MakerOrder || tr.Price != mk.price {
				tb.Fatalf("time priority violated: trade %+v, head of its queue is %v", tr, q)
			}
			mk.filled += tr.Qty
		case EvOrder:
			o, known := m.orders[ev.OrderID]
			if !known {
				if ev.Resting {
					m.orders[ev.OrderID] = &modelOrder{side: ev.Side, price: ev.Price, qty: ev.Qty, filled: ev.FilledQty}
					m.push(ev.OrderID)
				}
				continue
			}
			o.filled = ev.FilledQty
			if !ev.Resting {
				m.unqueue(ev.OrderID)
				delete(m.orders, ev.OrderID)
			}
		case EvReplaced:
			o := m.orders[ev.OrderID]
			if o == nil {
				tb.Fatalf("replaced an order the model does not know: %+v", ev)
			}
			if ev.PrevPrice != o.price || ev.PrevQty != o.qty || ev.PrevFilled != o.filled {
				tb.Fatalf("replaced report has wrong 'previous' state %+v, model has %+v", ev, *o)
			}
			if ev.Price == o.price && ev.Qty < o.qty {
				// Only legal in-place change: shrink at the same price.
				if !ev.Resting || ev.FilledQty != o.filled {
					tb.Fatalf("pure reduce must leave the order resting and untouched: %+v", ev)
				}
				o.qty = ev.Qty
				continue
			}
			m.unqueue(ev.OrderID)
			o.price, o.qty, o.filled = ev.Price, ev.Qty, ev.FilledQty
			if ev.Resting {
				m.push(ev.OrderID)
			} else {
				delete(m.orders, ev.OrderID)
			}
		}
	}
}

// check compares the engine's real book with the model, level by level, in
// queue order.
func (m *bookModel) check(tb testing.TB, e *Engine) {
	tb.Helper()
	real := map[modelKey][]uuid.UUID{}
	leaves := map[uuid.UUID]int64{}
	if b := e.Book("AAPL"); b != nil {
		for _, side := range []Side{Buy, Sell} {
			b.side(side).ascend(func(l *level) bool {
				for o := l.head; o != nil; o = o.next {
					k := modelKey{side, l.price}
					real[k] = append(real[k], o.id)
					leaves[o.id] = o.leaves()
				}
				return true
			})
		}
	}
	if fmt.Sprint(real) != fmt.Sprint(m.queues) {
		tb.Fatalf("book diverged from the model\nengine: %v\nmodel:  %v", real, m.queues)
	}
	for id, lv := range leaves {
		if mo := m.orders[id]; mo.qty-mo.filled != lv {
			tb.Fatalf("order %s leaves: engine %d, model %d", id, lv, mo.qty-mo.filled)
		}
	}
}

// runReplaceStream feeds cmds through a fresh engine, verifying the model and
// all structural invariants after every command, and returns every event.
func runReplaceStream(tb testing.TB, cmds []Command, kind IndexKind) []Event {
	tb.Helper()
	e := New(WithIndex(kind))
	m := newBookModel()
	var all []Event
	filled := map[uuid.UUID]int64{} // trade-derived fills per order
	for _, c := range cmds {
		evs := e.Process(c)
		if err := e.CheckInvariants(); err != nil {
			tb.Fatalf("invariants after %+v: %v", c, err)
		}
		for _, ev := range evs {
			switch ev.Kind {
			case EvTrade:
				filled[ev.Trade.BuyOrderID] += ev.Trade.Qty
				filled[ev.Trade.SellOrderID] += ev.Trade.Qty
				if ev.Trade.BuyLimit != 0 && ev.Trade.BuyLimit < ev.Trade.Price {
					tb.Fatalf("a buy order traded above its own limit: %+v", ev.Trade)
				}
			case EvReplaced:
				if ev.FilledQty != filled[ev.OrderID] {
					tb.Fatalf("replaced order %s reports filled=%d, trades sum to %d", ev.OrderID, ev.FilledQty, filled[ev.OrderID])
				}
				if ev.FilledQty > ev.Qty || (ev.Resting && ev.LeavesQty != ev.Qty-ev.FilledQty) {
					tb.Fatalf("inconsistent replaced report %+v", ev)
				}
			}
		}
		m.apply(tb, evs)
		m.check(tb, e)
		all = append(all, evs...)
	}
	return all
}

func TestPropertyReplaceStream(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		users := []uuid.UUID{uid("u1"), uid("u2"), uid("u3")}
		nCmds := rapid.IntRange(30, 160).Draw(rt, "nCmds")

		type known struct{ id, user uuid.UUID }
		var orders []known
		var cmds []Command
		var lastReplace *Command

		for i := 0; i < nCmds; i++ {
			var cmdID [16]byte
			binary.BigEndian.PutUint64(cmdID[8:], uint64(i+1))
			base := Command{ID: uuid.UUID(cmdID), Symbol: "AAPL", Timestamp: int64(i+1) * 1000}

			action := rapid.IntRange(0, 99).Draw(rt, "action")
			switch {
			case action < 45 || len(orders) == 0: // place a limit order
				var ob [16]byte
				binary.BigEndian.PutUint64(ob[:8], 0xfeedbeef)
				binary.BigEndian.PutUint64(ob[8:], uint64(i+1))
				c := base
				c.Kind, c.OrderID = CmdPlace, uuid.UUID(ob)
				c.UserID = users[rapid.IntRange(0, len(users)-1).Draw(rt, "user")]
				c.Side = Buy
				if rapid.Bool().Draw(rt, "sell") {
					c.Side = Sell
				}
				c.Type = Limit
				c.Price = rapid.Int64Range(98, 102).Draw(rt, "price")
				c.Qty = rapid.Int64Range(1, 30).Draw(rt, "qty")
				orders = append(orders, known{c.OrderID, c.UserID})
				cmds = append(cmds, c)
			case action < 55: // cancel
				k := orders[rapid.IntRange(0, len(orders)-1).Draw(rt, "cancelIdx")]
				c := base
				c.Kind, c.OrderID, c.UserID = CmdCancel, k.id, k.user
				cmds = append(cmds, c)
			case action < 60 && lastReplace != nil: // outbox re-publishes a replace
				cmds = append(cmds, *lastReplace)
			default: // replace (sometimes of a dead or foreign order, sometimes a no-op)
				k := orders[rapid.IntRange(0, len(orders)-1).Draw(rt, "replaceIdx")]
				c := base
				c.Kind, c.OrderID, c.UserID = CmdReplace, k.id, k.user
				if rapid.IntRange(0, 9).Draw(rt, "foreign") == 0 {
					c.UserID = users[rapid.IntRange(0, len(users)-1).Draw(rt, "otherUser")]
				}
				c.Price = rapid.Int64Range(98, 102).Draw(rt, "newPrice")
				c.Qty = rapid.Int64Range(1, 30).Draw(rt, "newQty")
				cp := c
				lastReplace = &cp
				cmds = append(cmds, c)
			}
		}

		evBTree := runReplaceStream(t, cmds, IndexBTree)
		evHeap := runReplaceStream(t, cmds, IndexHeap)
		if len(evBTree) != len(evHeap) {
			rt.Fatalf("event count mismatch btree=%d heap=%d", len(evBTree), len(evHeap))
		}
		for i := range evBTree {
			if evBTree[i] != evHeap[i] {
				rt.Fatalf("event %d differs between indexes:\nbtree: %+v\nheap:  %+v", i, evBTree[i], evHeap[i])
			}
		}

		// Snapshot + restore mid-stream must be indistinguishable from an
		// uninterrupted run, replaces included.
		split := len(cmds) / 2
		first := New()
		var out []Event
		for _, c := range cmds[:split] {
			out = append(out, first.Process(c)...)
		}
		blob, err := first.Snapshot()
		if err != nil {
			rt.Fatalf("snapshot: %v", err)
		}
		second, err := Restore(blob)
		if err != nil {
			rt.Fatalf("restore: %v", err)
		}
		for _, c := range cmds[split:] {
			out = append(out, second.Process(c)...)
		}
		if len(out) != len(evBTree) {
			rt.Fatalf("restored run emitted %d events, uninterrupted %d", len(out), len(evBTree))
		}
		for i := range out {
			if out[i] != evBTree[i] {
				rt.Fatalf("restored run diverges at event %d:\n%+v\n%+v", i, out[i], evBTree[i])
			}
		}
	})
}

func TestStopOrderTriggeredByTrade(t *testing.T) {
	h := newHarness(t)

	// Seed an initial trade at 10000 ($100.00) to establish lastTradePrice = 10000.
	h.limit(alice, "init-sell", Sell, 10000, 10)
	h.limit(bob, "init-buy", Buy, 10000, 10)
	if got := h.e.Book("AAPL").lastTradePrice; got != 10000 {
		t.Fatalf("expected lastTradePrice 10000, got %d", got)
	}

	// Place resting ask liquidity at 10500 and 10600.
	h.limit(alice, "ask-105", Sell, 10500, 5)
	h.limit(alice, "ask-106", Sell, 10600, 15)

	// Place a BUY STOP with StopPrice=10500 and protection Price=11000 for Qty=12.
	evs := h.do(Command{
		Kind:      CmdPlace,
		OrderID:   uid("stop-buy-1"),
		UserID:    carol,
		Side:      Buy,
		Type:      Stop,
		Price:     11000,
		StopPrice: 10500,
		Qty:       12,
	})
	if len(evs) != 1 || evs[0].Kind != EvOrder || evs[0].Status != StatusAccepted || evs[0].Resting {
		t.Fatalf("expected single non-resting StatusAccepted for untriggered stop, got %+v", evs)
	}

	// Trigger the stop by buying the 5 shares at 10500 -> lastTradePrice becomes 10500 >= StopPrice(10500).
	triggerEvs := h.limit(bob, "trigger-buy", Buy, 10500, 5)

	var stopFilled bool
	var stopTradedQty int64
	for _, ev := range triggerEvs {
		if ev.Kind == EvTrade && ev.Trade.BuyOrderID == uid("stop-buy-1") {
			stopTradedQty += ev.Trade.Qty
			if ev.Trade.Price != 10600 {
				t.Fatalf("expected stop trade at 10600, got %d", ev.Trade.Price)
			}
		}
		if ev.Kind == EvOrder && ev.OrderID == uid("stop-buy-1") && ev.Status == StatusFilled {
			stopFilled = true
			if ev.Type != Stop || ev.StopPrice != 10500 {
				t.Fatalf("expected original Stop type and StopPrice preserved on fill, got %+v", ev)
			}
		}
	}
	if !stopFilled || stopTradedQty != 12 {
		t.Fatalf("expected stop-buy-1 to fill 12 shares, filled=%v qty=%d evs=%+v", stopFilled, stopTradedQty, triggerEvs)
	}
}

func TestStopLimitOrderRestingAfterTrigger(t *testing.T) {
	h := newHarness(t)

	// Seed lastTradePrice = 10000.
	h.limit(alice, "s1", Sell, 10000, 10)
	h.limit(bob, "b1", Buy, 10000, 10)

	// Place resting bids at 9500 (qty 5) and 9300 (qty 10).
	h.limit(bob, "bid-95", Buy, 9500, 5)
	h.limit(bob, "bid-93", Buy, 9300, 10)

	// Place a SELL STOP_LIMIT: triggers at StopPrice=9500, limit Price=9400, Qty=12.
	evs := h.do(Command{
		Kind:      CmdPlace,
		OrderID:   uid("sl-1"),
		UserID:    carol,
		Side:      Sell,
		Type:      StopLimit,
		Price:     9400,
		StopPrice: 9500,
		Qty:       12,
	})
	if len(evs) != 1 || evs[0].Kind != EvOrder || evs[0].Status != StatusAccepted || evs[0].Resting {
		t.Fatalf("expected non-resting StatusAccepted for stop-limit, got %+v", evs)
	}

	// Sell 5 shares into bid-95 -> trade at 9500 triggers sl-1.
	// However, bid-95 is exhausted by the triggering trade, and the next bid is 9300 < limit 9400.
	// So sl-1 must rest on the ask book at 9400 for all 12 shares!
	trigEvs := h.limit(alice, "trig-sell", Sell, 9500, 5)

	rep := last(t, trigEvs, "sl-1")
	if rep.Status != StatusAccepted || !rep.Resting || rep.Price != 9400 || rep.LeavesQty != 12 {
		t.Fatalf("expected triggered stop-limit to rest on book at 9400, got %+v", rep)
	}
	snap := h.e.Book("AAPL").Depth(5)
	if len(snap.Asks) != 1 || snap.Asks[0].Price != 9400 || snap.Asks[0].Qty != 12 {
		t.Fatalf("expected ask depth of 12 @ 9400, got %+v", snap.Asks)
	}
}

func TestStopOrderCancellationAndRejection(t *testing.T) {
	h := newHarness(t)

	// Seed lastTradePrice = 10000.
	h.limit(alice, "s1", Sell, 10000, 10)
	h.limit(bob, "b1", Buy, 10000, 10)

	// BUY STOP at StopPrice=9900 (already <= 10000) must be rejected with ReasonStopHit.
	rejBuy := h.do(Command{
		Kind: CmdPlace, OrderID: uid("bad-buy-stop"),
		UserID: alice, Side: Buy, Type: Stop, StopPrice: 9900, Qty: 5,
	})
	if len(rejBuy) != 1 || rejBuy[0].Kind != EvOrder || rejBuy[0].Status != StatusRejected || rejBuy[0].Reason != ReasonStopHit {
		t.Fatalf("expected ReasonStopHit on already-crossed buy stop, got %+v", rejBuy)
	}

	// SELL STOP at StopPrice=10100 (already >= 10000) must be rejected with ReasonStopHit.
	rejSell := h.do(Command{
		Kind: CmdPlace, OrderID: uid("bad-sell-stop"),
		UserID: alice, Side: Sell, Type: Stop, StopPrice: 10100, Qty: 5,
	})
	if len(rejSell) != 1 || rejSell[0].Kind != EvOrder || rejSell[0].Status != StatusRejected || rejSell[0].Reason != ReasonStopHit {
		t.Fatalf("expected ReasonStopHit on already-crossed sell stop, got %+v", rejSell)
	}

	// Valid SELL STOP at StopPrice=9500, then cancel before trigger.
	h.do(Command{
		Kind: CmdPlace, OrderID: uid("cancel-me"),
		UserID: alice, Side: Sell, Type: Stop, StopPrice: 9500, Qty: 8,
	})
	cancEvs := h.cancel(alice, "cancel-me")
	if len(cancEvs) != 1 || cancEvs[0].Kind != EvOrder || cancEvs[0].Status != StatusCancelled || cancEvs[0].LeavesQty != 0 {
		t.Fatalf("expected StatusCancelled on untriggered stop, got %+v", cancEvs)
	}
}

func TestStopOrderSnapshotRestore(t *testing.T) {
	cmds := []Command{
		{ID: uid("c1"), Kind: CmdPlace, Symbol: "AAPL", OrderID: uid("s1"), UserID: alice, Side: Sell, Type: Limit, Price: 10000, Qty: 10, Timestamp: 1},
		{ID: uid("c2"), Kind: CmdPlace, Symbol: "AAPL", OrderID: uid("b1"), UserID: bob, Side: Buy, Type: Limit, Price: 10000, Qty: 10, Timestamp: 2},
		{ID: uid("c3"), Kind: CmdPlace, Symbol: "AAPL", OrderID: uid("ask-105"), UserID: alice, Side: Sell, Type: Limit, Price: 10500, Qty: 10, Timestamp: 3},
		{ID: uid("c4"), Kind: CmdPlace, Symbol: "AAPL", OrderID: uid("ask-108"), UserID: alice, Side: Sell, Type: Limit, Price: 10800, Qty: 20, Timestamp: 4},
		{ID: uid("c5"), Kind: CmdPlace, Symbol: "AAPL", OrderID: uid("stop-1"), UserID: carol, Side: Buy, Type: Stop, Price: 11000, StopPrice: 10500, Qty: 6, Timestamp: 5},
		{ID: uid("c6"), Kind: CmdPlace, Symbol: "AAPL", OrderID: uid("stop-2"), UserID: carol, Side: Buy, Type: StopLimit, Price: 10700, StopPrice: 10500, Qty: 8, Timestamp: 6},
		// Snapshot taken right before the triggering trade at 10500:
		{ID: uid("c7"), Kind: CmdPlace, Symbol: "AAPL", OrderID: uid("trig"), UserID: bob, Side: Buy, Type: Limit, Price: 10500, Qty: 4, Timestamp: 7},
	}

	full := New()
	var fullEvs []Event
	for _, c := range cmds {
		fullEvs = append(fullEvs, full.Process(c)...)
	}

	split := len(cmds) - 1
	part1 := New()
	var splitEvs []Event
	for _, c := range cmds[:split] {
		splitEvs = append(splitEvs, part1.Process(c)...)
	}
	blob, err := part1.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	part2, err := Restore(blob)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if err := part2.CheckInvariants(); err != nil {
		t.Fatalf("restored invariants: %v", err)
	}
	for _, c := range cmds[split:] {
		splitEvs = append(splitEvs, part2.Process(c)...)
	}

	if len(fullEvs) != len(splitEvs) {
		t.Fatalf("event count mismatch full=%d restored=%d", len(fullEvs), len(splitEvs))
	}
	for i := range fullEvs {
		if fullEvs[i] != splitEvs[i] {
			t.Fatalf("event %d differs:\nfull:     %+v\nrestored: %+v", i, fullEvs[i], splitEvs[i])
		}
	}
}
