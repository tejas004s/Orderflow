package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/google/uuid"
	"github.com/tidwall/btree"
)

const snapshotVersion = 1

type snapshot struct {
	Version     int        `json:"v"`
	DedupeWin   int64      `json:"dedupe_window"`
	DedupeMaxTS int64      `json:"dedupe_max_ts"`
	Seen        []seenSnap `json:"seen"`
	Books       []bookSnap `json:"books"`
}

type seenSnap struct {
	ID uuid.UUID `json:"id"`
	TS int64     `json:"ts"`
}

type bookSnap struct {
	Symbol string `json:"symbol"`
	Seq    uint64 `json:"seq"`
	// LastPx is the last trade price, which decides when stops trigger. It is
	// part of the state because a restored engine must trigger exactly the
	// stops an uninterrupted one would.
	LastPx int64 `json:"last_px,omitempty"`
	// Orders in priority order: bids best->worst (FIFO within a level),
	// then asks best->worst. Re-inserting in this order restores priority.
	Orders []orderSnap `json:"orders"`
	// Stops is the trigger book: buy stops by ascending trigger price, then
	// sell stops by descending trigger price, FIFO within a price.
	Stops []stopSnap `json:"stops,omitempty"`
}

type stopSnap struct {
	ID    uuid.UUID `json:"id"`
	User  uuid.UUID `json:"user"`
	Side  Side      `json:"side"`
	Type  OrderType `json:"type"`
	Price int64     `json:"px"`
	Stop  int64     `json:"stop"`
	Qty   int64     `json:"qty"`
	TS    int64     `json:"ts"`
}

type orderSnap struct {
	ID     uuid.UUID `json:"id"`
	User   uuid.UUID `json:"user"`
	Side   Side      `json:"side"`
	Price  int64     `json:"px"`
	Qty    int64     `json:"qty"`
	Filled int64     `json:"filled"`
	TS     int64     `json:"ts"`
}

// Snapshot serialises the full engine state deterministically.
func (e *Engine) Snapshot() ([]byte, error) {
	s := snapshot{Version: snapshotVersion, DedupeWin: e.seen.window, DedupeMaxTS: e.seen.maxTS}
	for _, se := range e.seen.entries() {
		s.Seen = append(s.Seen, seenSnap{se.id, se.ts})
	}
	syms := make([]string, 0, len(e.books))
	for sym := range e.books {
		syms = append(syms, sym)
	}
	sort.Strings(syms)
	for _, sym := range syms {
		b := e.books[sym]
		bs := bookSnap{Symbol: sym, Seq: b.seq, LastPx: b.lastTradePrice, Orders: make([]orderSnap, 0, len(b.orders))}
		for _, idx := range []levelIndex{b.bids, b.asks} {
			idx.ascend(func(l *level) bool {
				for o := l.head; o != nil; o = o.next {
					bs.Orders = append(bs.Orders, orderSnap{o.id, o.user, o.side, o.price, o.qty, o.filled, o.ts})
				}
				return true
			})
		}
		for _, tree := range []*btree.BTreeG[*stopLevel]{b.buyStops, b.sellStops} {
			tree.Scan(func(l *stopLevel) bool {
				for o := l.head; o != nil; o = o.next {
					bs.Stops = append(bs.Stops, stopSnap{o.id, o.user, o.side, o.typ, o.price, o.stopPrice, o.qty, o.ts})
				}
				return true
			})
		}
		s.Books = append(s.Books, bs)
	}
	return json.Marshal(s)
}

// Restore rebuilds an engine from a Snapshot.
func Restore(data []byte, opts ...Option) (*Engine, error) {
	var s snapshot
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("decode snapshot: %w", err)
	}
	if s.Version != snapshotVersion {
		return nil, fmt.Errorf("unsupported snapshot version %d", s.Version)
	}
	e := New(append([]Option{WithDedupeWindow(s.DedupeWin)}, opts...)...)
	e.seen.maxTS = s.DedupeMaxTS
	for _, se := range s.Seen {
		e.seen.ids[se.ID] = struct{}{}
		e.seen.queue = append(e.seen.queue, seenEntry{se.ID, se.TS})
	}
	for _, bs := range s.Books {
		b := e.book(bs.Symbol)
		b.seq = bs.Seq
		b.lastTradePrice = bs.LastPx
		for _, os := range bs.Orders {
			b.rest(&order{id: os.ID, user: os.User, side: os.Side, price: os.Price,
				qty: os.Qty, filled: os.Filled, ts: os.TS})
		}
		for _, ss := range bs.Stops {
			b.restStop(&stopOrder{id: ss.ID, user: ss.User, side: ss.Side, typ: ss.Type,
				price: ss.Price, stopPrice: ss.Stop, qty: ss.Qty, ts: ss.TS})
		}
	}
	return e, e.CheckInvariants()
}

// CheckInvariants verifies the structural invariants of every book
// (SDD section 5.5). It is O(orders) and intended for tests and debugging.
func (e *Engine) CheckInvariants() error {
	var errs []error
	for sym, b := range e.books {
		if bid, ok := b.BestBid(); ok {
			if ask, ok := b.BestAsk(); ok && bid >= ask {
				errs = append(errs, fmt.Errorf("%s: crossed book bid=%d ask=%d", sym, bid, ask))
			}
		}
		count := 0
		for _, side := range []Side{Buy, Sell} {
			idx := b.side(side)
			levels := 0
			var prev int64
			first := true
			idx.ascend(func(l *level) bool {
				levels++
				if !first && ((side == Buy && l.price >= prev) || (side == Sell && l.price <= prev)) {
					errs = append(errs, fmt.Errorf("%s: levels out of order", sym))
				}
				first, prev = false, l.price
				var total int64
				n := 0
				for o := l.head; o != nil; o = o.next {
					n++
					if o.lvl != l || o.side != side || o.price != l.price {
						errs = append(errs, fmt.Errorf("%s: order %s misplaced", sym, o.id))
					}
					if o.leaves() <= 0 || o.filled < 0 || o.filled > o.qty {
						errs = append(errs, fmt.Errorf("%s: order %s bad fill %d/%d", sym, o.id, o.filled, o.qty))
					}
					if b.orders[o.id] != o {
						errs = append(errs, fmt.Errorf("%s: order %s not indexed", sym, o.id))
					}
					total += o.leaves()
				}
				if n == 0 || n != l.count || total != l.total {
					errs = append(errs, fmt.Errorf("%s: level %d count/total mismatch", sym, l.price))
				}
				count += n
				return true
			})
			if levels != idx.len() {
				errs = append(errs, fmt.Errorf("%s: level index size mismatch", sym))
			}
		}
		if count != len(b.orders) {
			errs = append(errs, fmt.Errorf("%s: %d orders in levels, %d indexed", sym, count, len(b.orders)))
		}
		errs = append(errs, b.checkStopInvariants()...)
	}
	return errors.Join(errs...)
}

// checkStopInvariants verifies the trigger book: every held stop is indexed,
// sits at the right price level and side, and has NOT already been crossed by
// the last trade price (such a stop would have been triggered).
func (b *Book) checkStopInvariants() []error {
	var errs []error
	sym := b.Symbol
	count := 0
	for _, side := range []Side{Buy, Sell} {
		tree, byPx := b.buyStops, b.buyStopsByPx
		if side == Sell {
			tree, byPx = b.sellStops, b.sellStopsByPx
		}
		levels := 0
		tree.Scan(func(l *stopLevel) bool {
			levels++
			if byPx[l.stopPrice] != l {
				errs = append(errs, fmt.Errorf("%s: stop level %d not indexed", sym, l.stopPrice))
			}
			n := 0
			for o := l.head; o != nil; o = o.next {
				n++
				if o.lvl != l || o.side != side || o.stopPrice != l.stopPrice {
					errs = append(errs, fmt.Errorf("%s: stop %s misplaced", sym, o.id))
				}
				if b.stopOrders[o.id] != o {
					errs = append(errs, fmt.Errorf("%s: stop %s not indexed", sym, o.id))
				}
				if o.typ != Stop && o.typ != StopLimit {
					errs = append(errs, fmt.Errorf("%s: stop %s has type %v", sym, o.id, o.typ))
				}
				if last := b.lastTradePrice; last > 0 &&
					((side == Buy && last >= o.stopPrice) || (side == Sell && last <= o.stopPrice)) {
					errs = append(errs, fmt.Errorf("%s: stop %s (%v @ %d) should have triggered at last price %d",
						sym, o.id, side, o.stopPrice, last))
				}
			}
			if n == 0 || n != l.count {
				errs = append(errs, fmt.Errorf("%s: stop level %d count mismatch", sym, l.stopPrice))
			}
			count += n
			return true
		})
		if levels != len(byPx) {
			errs = append(errs, fmt.Errorf("%s: stop level index size mismatch", sym))
		}
	}
	if count != len(b.stopOrders) {
		errs = append(errs, fmt.Errorf("%s: %d stops in levels, %d indexed", sym, count, len(b.stopOrders)))
	}
	return errs
}
