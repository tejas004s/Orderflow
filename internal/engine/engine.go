package engine

import (
	"encoding/binary"

	"github.com/google/uuid"
)

// nsEvent is the UUIDv5 namespace for engine output event IDs.
var nsEvent = uuid.MustParse("6f1c1f5e-3b7a-4d0e-9a51-0e8f3c1d2b40")

// DefaultDedupeWindow is how long (in command time) processed order IDs are
// remembered to discard re-published place commands. See ADR-002.
const DefaultDedupeWindow = int64(10 * 60 * 1e9) // 10 minutes in ns

// Option configures an Engine.
type Option func(*Engine)

// WithIndex selects the price-level index implementation.
func WithIndex(k IndexKind) Option { return func(e *Engine) { e.index = k } }

// WithDedupeWindow sets the duplicate-detection window in nanoseconds.
func WithDedupeWindow(ns int64) Option { return func(e *Engine) { e.seen.window = ns } }

// Engine owns the books of all symbols routed to it.
type Engine struct {
	books map[string]*Book
	seen  dedupe
	index IndexKind

	// per-command scratch state
	out  []Event
	cmd  *Command
	nOut uint32
}

// New creates an empty engine.
func New(opts ...Option) *Engine {
	e := &Engine{books: map[string]*Book{}, seen: newDedupe(DefaultDedupeWindow)}
	for _, o := range opts {
		o(e)
	}
	return e
}

// Book returns the book for symbol, or nil if none exists yet.
func (e *Engine) Book(symbol string) *Book { return e.books[symbol] }

// Symbols returns the number of books.
func (e *Engine) Symbols() int { return len(e.books) }

func (e *Engine) book(symbol string) *Book {
	b := e.books[symbol]
	if b == nil {
		b = newBook(symbol, e.index)
		e.books[symbol] = b
	}
	return b
}

// Process applies one command and returns the resulting events. The
// returned slice is owned by the engine and only valid until the next
// call; copy it if you need to retain it.
func (e *Engine) Process(cmd Command) []Event {
	e.out = e.out[:0]
	e.cmd = &cmd
	e.nOut = 0
	switch cmd.Kind {
	case CmdPlace:
		e.place(&cmd)
	case CmdCancel:
		e.cancel(&cmd)
	default:
		e.emitOrder(nil, &cmd, StatusRejected, ReasonInvalid, 0, false)
	}
	e.cmd = nil
	return e.out
}

func validPlace(c *Command) bool {
	if c.Symbol == "" || c.OrderID == uuid.Nil || c.Qty <= 0 {
		return false
	}
	if c.Side != Buy && c.Side != Sell {
		return false
	}
	switch c.Type {
	case Limit:
		return c.Price > 0
	case Market:
		return c.Price >= 0
	}
	return false
}

func (e *Engine) place(c *Command) {
	if !validPlace(c) {
		var b *Book
		if c.Symbol != "" {
			b = e.book(c.Symbol)
		}
		e.emitOrder(b, c, StatusRejected, ReasonInvalid, 0, false)
		return
	}
	b := e.book(c.Symbol)
	if _, resting := b.orders[c.OrderID]; resting || !e.seen.add(c.OrderID, c.Timestamp) {
		e.emit(b, Event{Kind: EvDuplicate, OrderID: c.OrderID, UserID: c.UserID,
			Side: c.Side, Type: c.Type, Price: c.Price, Qty: c.Qty, Reason: ReasonDuplicateOrder})
		return
	}

	taker := &order{id: c.OrderID, user: c.UserID, side: c.Side, price: c.Price, qty: c.Qty, ts: c.Timestamp}
	opp := b.side(c.Side.Opposite())
	stp := false
	bandHit := false

	for taker.leaves() > 0 {
		lvl := opp.best()
		if lvl == nil {
			break
		}
		if !crosses(c, lvl.price) {
			bandHit = c.Type == Market
			break
		}
		maker := lvl.head
		if maker.user == taker.user {
			stp = true // self-trade prevention: cancel the incoming order
			break
		}
		qty := min(taker.leaves(), maker.leaves())
		e.trade(b, taker, maker, lvl.price, qty)
	}

	switch {
	case taker.leaves() == 0:
		e.emitOrder(b, c, StatusFilled, ReasonNone, taker.filled, false)
	case stp:
		e.emitOrder(b, c, StatusCancelled, ReasonSelfTrade, taker.filled, false)
	case c.Type == Market:
		reason := ReasonIOCRemainder
		if bandHit {
			reason = ReasonPriceBand
		}
		if taker.filled == 0 {
			if !bandHit {
				reason = ReasonNoLiquidity
			}
			e.emitOrder(b, c, StatusRejected, reason, 0, false)
		} else {
			e.emitOrder(b, c, StatusCancelled, reason, taker.filled, false)
		}
	default: // limit remainder rests (GTC)
		b.rest(taker)
		st := StatusAccepted
		if taker.filled > 0 {
			st = StatusPartiallyFilled
		}
		e.emitOrder(b, c, st, ReasonNone, taker.filled, true)
	}
}

// crosses reports whether an incoming order may trade at the resting price.
func crosses(c *Command, restingPx int64) bool {
	limit := c.Price
	if c.Type == Market && limit == 0 {
		return true
	}
	if c.Side == Buy {
		return restingPx <= limit
	}
	return restingPx >= limit
}

func (e *Engine) trade(b *Book, taker, maker *order, px, qty int64) {
	lvl := maker.lvl
	maker.filled += qty
	lvl.total -= qty
	taker.filled += qty

	t := Trade{Price: px, Qty: qty, MakerOrder: maker.id, Aggressor: taker.side}
	if taker.side == Buy {
		t.BuyOrderID, t.BuyerID, t.SellOrderID, t.SellerID = taker.id, taker.user, maker.id, maker.user
	} else {
		t.BuyOrderID, t.BuyerID, t.SellOrderID, t.SellerID = maker.id, maker.user, taker.id, taker.user
	}
	e.emit(b, Event{Kind: EvTrade, Trade: t})

	// Maker execution report.
	ev := Event{
		Kind: EvOrder, OrderID: maker.id, UserID: maker.user, Side: maker.side, Type: Limit,
		Price: maker.price, Qty: maker.qty, FilledQty: maker.filled, LeavesQty: maker.leaves(),
	}
	if maker.leaves() == 0 {
		b.unlink(maker)
		ev.Status = StatusFilled
		ev.LeavesQty = 0
	} else {
		ev.Status = StatusPartiallyFilled
		ev.Resting = true
	}
	e.emit(b, ev)
}

func (e *Engine) cancel(c *Command) {
	if c.Symbol == "" || c.OrderID == uuid.Nil {
		e.emit(nil, Event{Kind: EvCancelReject, OrderID: c.OrderID, UserID: c.UserID, Reason: ReasonInvalid})
		return
	}
	b := e.book(c.Symbol)
	o, ok := b.orders[c.OrderID]
	if !ok || (c.UserID != uuid.Nil && o.user != c.UserID) {
		e.emit(b, Event{Kind: EvCancelReject, OrderID: c.OrderID, UserID: c.UserID, Reason: ReasonUnknownOrder})
		return
	}
	b.unlink(o)
	e.emit(b, Event{
		Kind: EvOrder, OrderID: o.id, UserID: o.user, Side: o.side, Type: Limit, Price: o.price,
		Qty: o.qty, FilledQty: o.filled, LeavesQty: 0, Status: StatusCancelled, Reason: ReasonUserCancel,
	})
}

// emitOrder emits an execution report for the incoming order of command c.
func (e *Engine) emitOrder(b *Book, c *Command, st OrderStatus, r Reason, filled int64, resting bool) {
	leaves := int64(0)
	if resting {
		leaves = c.Qty - filled
	}
	e.emit(b, Event{
		Kind: EvOrder, OrderID: c.OrderID, UserID: c.UserID, Side: c.Side, Type: c.Type,
		Price: c.Price, Qty: c.Qty, FilledQty: filled, LeavesQty: leaves, Status: st, Reason: r,
		Resting: resting,
	})
}

// emit stamps the envelope fields (deterministic ID, sequence, timestamp).
func (e *Engine) emit(b *Book, ev Event) {
	var buf [20]byte
	copy(buf[:16], e.cmd.ID[:])
	binary.BigEndian.PutUint32(buf[16:], e.nOut)
	e.nOut++
	ev.ID = uuid.NewSHA1(nsEvent, buf[:])
	ev.CommandID = e.cmd.ID
	ev.Timestamp = e.cmd.Timestamp
	if b != nil {
		b.seq++
		ev.Seq = b.seq
		ev.Symbol = b.Symbol
	} else {
		ev.Symbol = e.cmd.Symbol
	}
	if ev.Kind == EvTrade {
		ev.Trade.MatchID = ev.ID
	}
	e.out = append(e.out, ev)
}

// ---- duplicate detection ----------------------------------------------------

// dedupe remembers order IDs seen within a sliding window of command time.
// It is part of the deterministic state (and the snapshot).
type dedupe struct {
	window int64
	ids    map[uuid.UUID]struct{}
	queue  []seenEntry // FIFO in arrival order
	head   int
	maxTS  int64
}

type seenEntry struct {
	id uuid.UUID
	ts int64
}

func newDedupe(window int64) dedupe {
	return dedupe{window: window, ids: map[uuid.UUID]struct{}{}}
}

// add records id and returns false if it was already present.
func (d *dedupe) add(id uuid.UUID, ts int64) bool {
	if _, ok := d.ids[id]; ok {
		return false
	}
	if ts > d.maxTS {
		d.maxTS = ts
	}
	cutoff := d.maxTS - d.window
	for d.head < len(d.queue) && d.queue[d.head].ts < cutoff {
		delete(d.ids, d.queue[d.head].id)
		d.head++
	}
	if d.head > 4096 && d.head*2 > len(d.queue) { // compact
		n := copy(d.queue, d.queue[d.head:])
		d.queue = d.queue[:n]
		d.head = 0
	}
	d.ids[id] = struct{}{}
	d.queue = append(d.queue, seenEntry{id, ts})
	return true
}

func (d *dedupe) entries() []seenEntry { return d.queue[d.head:] }
