package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/google/uuid"
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
	// Orders in priority order: bids best->worst (FIFO within a level),
	// then asks best->worst. Re-inserting in this order restores priority.
	Orders []orderSnap `json:"orders"`
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
		bs := bookSnap{Symbol: sym, Seq: b.seq, Orders: make([]orderSnap, 0, len(b.orders))}
		for _, idx := range []levelIndex{b.bids, b.asks} {
			idx.ascend(func(l *level) bool {
				for o := l.head; o != nil; o = o.next {
					bs.Orders = append(bs.Orders, orderSnap{o.id, o.user, o.side, o.price, o.qty, o.filled, o.ts})
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
		for _, os := range bs.Orders {
			b.rest(&order{id: os.ID, user: os.User, side: os.Side, price: os.Price,
				qty: os.Qty, filled: os.Filled, ts: os.TS})
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
	}
	return errors.Join(errs...)
}
