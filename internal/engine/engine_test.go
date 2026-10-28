package engine

import (
	"fmt"
	"testing"

	"github.com/google/uuid"
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
