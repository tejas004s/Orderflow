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
	case CmdReplace:
		e.replace(&cmd)
	default:
		e.emitOrder(nil, &cmd, StatusRejected, ReasonInvalid, 0, false)
	}
	// Trades update the book's last trade price; any stop order it crossed is
	// elected only now, after the command's own matching is complete, so a
	// command is never interleaved with the orders it triggers. The check is
	// a map length lookup, so engines with no stop orders pay nothing.
	if b := e.books[cmd.Symbol]; b != nil && len(b.stopOrders) > 0 {
		e.triggerStops(b)
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
	if c.Type != Stop && c.Type != StopLimit && c.StopPrice != 0 {
		return false // only stop orders carry a trigger price
	}
	switch c.Type {
	case Limit:
		return c.Price > 0
	case Market:
		return c.Price >= 0
	case Stop:
		return c.StopPrice > 0 && c.Price >= 0
	case StopLimit:
		return c.StopPrice > 0 && c.Price > 0
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
	_, resting := b.orders[c.OrderID]
	_, held := b.stopOrders[c.OrderID]
	if resting || held || !e.seen.add(c.OrderID, c.Timestamp) {
		e.emit(b, Event{Kind: EvDuplicate, OrderID: c.OrderID, UserID: c.UserID,
			Side: c.Side, Type: c.Type, Price: c.Price, Qty: c.Qty, Reason: ReasonDuplicateOrder})
		return
	}
	if c.Type == Stop || c.Type == StopLimit {
		e.placeStop(b, c)
		return
	}
	e.match(b, c, c.Type)
}

// placeStop parks a stop or stop-limit order in the trigger book.
//
// A stop whose trigger price has already been crossed by the last trade is
// rejected rather than fired on the spot: a BUY stop at or below the market, or
// a SELL stop at or above it, is almost always a typo, and executing it
// instantly would turn that typo into a market order. Until the first trade of
// a symbol there is no last trade price, so nothing can be crossed yet.
func (e *Engine) placeStop(b *Book, c *Command) {
	if last := b.lastTradePrice; last > 0 &&
		((c.Side == Buy && last >= c.StopPrice) || (c.Side == Sell && last <= c.StopPrice)) {
		e.emitOrder(b, c, StatusRejected, ReasonStopHit, 0, false)
		return
	}
	b.restStop(&stopOrder{id: c.OrderID, user: c.UserID, side: c.Side, typ: c.Type,
		price: c.Price, stopPrice: c.StopPrice, qty: c.Qty, ts: c.Timestamp})
	e.emit(b, Event{
		Kind: EvOrder, OrderID: c.OrderID, UserID: c.UserID, Side: c.Side, Type: c.Type,
		Price: c.Price, StopPrice: c.StopPrice, Qty: c.Qty, LeavesQty: c.Qty,
		Status: StatusAccepted, // held in the stop book: Resting stays false
	})
}

// triggerStops elects every stop order crossed by the last trade price and
// injects it into the active book, repeating until no stop is left to fire:
// the orders it injects can trade and move the price, which can fire more.
// Each pass removes at least one stop from the trigger book, so it terminates.
func (e *Engine) triggerStops(b *Book) {
	for {
		fired := b.electTriggeredStops()
		if len(fired) == 0 {
			return
		}
		for _, s := range fired {
			e.injectStop(b, s)
		}
	}
}

// injectStop turns a triggered stop into the order it was waiting to become (a
// STOP becomes a MARKET order, a STOP_LIMIT a LIMIT order) and runs it through
// the matching logic as an incoming order. It is stamped with the time of the
// command that triggered it, so a limit remainder queues behind orders that
// were already resting. Its reports keep the original STOP/STOP_LIMIT type.
func (e *Engine) injectStop(b *Book, s *stopOrder) {
	typ := Limit
	if s.typ == Stop {
		typ = Market
	}
	c := &Command{
		ID: e.cmd.ID, Kind: CmdPlace, Symbol: b.Symbol, OrderID: s.id, UserID: s.user,
		Side: s.side, Type: typ, Price: s.price, StopPrice: s.stopPrice, Qty: s.qty,
		Timestamp: e.cmd.Timestamp,
	}
	e.match(b, c, s.typ)
}

// match runs an incoming order through the opposite side of the book and
// emits its final execution report. c.Type decides how it trades (LIMIT or
// MARKET); reportAs is the order type its reports carry, which differs from
// c.Type only for a triggered stop.
func (e *Engine) match(b *Book, c *Command, reportAs OrderType) {
	rep := *c
	rep.Type = reportAs

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
		e.emitOrder(b, &rep, StatusFilled, ReasonNone, taker.filled, false)
	case stp:
		e.emitOrder(b, &rep, StatusCancelled, ReasonSelfTrade, taker.filled, false)
	case c.Type == Market:
		reason := ReasonIOCRemainder
		if bandHit {
			reason = ReasonPriceBand
		}
		if taker.filled == 0 {
			if !bandHit {
				reason = ReasonNoLiquidity
			}
			e.emitOrder(b, &rep, StatusRejected, reason, 0, false)
		} else {
			e.emitOrder(b, &rep, StatusCancelled, reason, taker.filled, false)
		}
	default: // limit remainder rests (GTC)
		b.rest(taker)
		st := StatusAccepted
		if taker.filled > 0 {
			st = StatusPartiallyFilled
		}
		e.emitOrder(b, &rep, st, ReasonNone, taker.filled, true)
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
		t.BuyLimit = taker.price
	} else {
		t.BuyOrderID, t.BuyerID, t.SellOrderID, t.SellerID = maker.id, maker.user, taker.id, taker.user
		t.BuyLimit = maker.price
	}
	e.emit(b, Event{Kind: EvTrade, Trade: t})
	b.lastTradePrice = px // arms the stop trigger check run after the command

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
	if s, held := b.stopOrders[c.OrderID]; held && (c.UserID == uuid.Nil || s.user == c.UserID) {
		// An un-triggered stop order: it never reached the book, so it has
		// nothing to unlink there and nothing was ever filled.
		b.unlinkStop(s)
		e.emit(b, Event{
			Kind: EvOrder, OrderID: s.id, UserID: s.user, Side: s.side, Type: s.typ, Price: s.price,
			StopPrice: s.stopPrice, Qty: s.qty, LeavesQty: 0, Status: StatusCancelled, Reason: ReasonUserCancel,
		})
		return
	}
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

// replace implements cancel-replace (see CmdReplace). The command carries the
// new limit price and the new TOTAL quantity of the order.
//
// Time priority is the order's position in its price level's FIFO queue:
//
//   - same price, qty reduced: the order is shrunk in place and keeps its slot;
//   - same price, qty increased: the order is moved to the tail of the level;
//   - price changed: the order is removed and re-inserted as if it were a new
//     limit order at the new price. It may trade immediately if the new price
//     crosses the book, in which case self-trade prevention applies exactly as
//     for a placed order (the replaced order is the one cancelled).
//
// On success the final event is EvReplaced, preceded by the trades (and maker
// reports) the replace triggered. On failure a single EvReplaceReject is
// emitted and nothing changes.
func (e *Engine) replace(c *Command) {
	reject := func(b *Book, r Reason) {
		e.emit(b, Event{Kind: EvReplaceReject, OrderID: c.OrderID, UserID: c.UserID,
			Price: c.Price, Qty: c.Qty, Reason: r})
	}
	if c.Symbol == "" || c.OrderID == uuid.Nil || c.Price <= 0 || c.Qty <= 0 {
		var b *Book
		if c.Symbol != "" {
			b = e.book(c.Symbol)
		}
		reject(b, ReasonInvalid)
		return
	}
	b := e.book(c.Symbol)
	// A re-delivered replace command must not be applied twice. Command IDs
	// share the dedupe window with order IDs; UUIDs never collide.
	if c.ID != uuid.Nil && !e.seen.add(c.ID, c.Timestamp) {
		e.emit(b, Event{Kind: EvDuplicate, OrderID: c.OrderID, UserID: c.UserID,
			Price: c.Price, Qty: c.Qty, Reason: ReasonDuplicateOrder})
		return
	}
	o, ok := b.orders[c.OrderID]
	if !ok || (c.UserID != uuid.Nil && o.user != c.UserID) {
		reject(b, ReasonUnknownOrder)
		return
	}
	if c.Qty <= o.filled {
		reject(b, ReasonQtyTooLow)
		return
	}
	if c.Price == o.price && c.Qty == o.qty {
		reject(b, ReasonNoChange)
		return
	}

	prevPrice, prevQty, prevFilled := o.price, o.qty, o.filled
	stp := false

	switch {
	case c.Price == o.price && c.Qty < o.qty:
		b.shrink(o, c.Qty) // keeps time priority
	case c.Price == o.price:
		// Qty increased at the same price: back of the queue. The level is
		// already on the book and uncrossed, so no matching is possible.
		b.unlink(o)
		o.qty, o.ts = c.Qty, c.Timestamp
		b.rest(o)
	default:
		b.unlink(o)
		o.price, o.qty, o.ts = c.Price, c.Qty, c.Timestamp
		opp := b.side(o.side.Opposite())
		for o.leaves() > 0 {
			lvl := opp.best()
			if lvl == nil || !crossesLimit(o.side, o.price, lvl.price) {
				break
			}
			maker := lvl.head
			if maker.user == o.user {
				stp = true // self-trade prevention: cancel the replaced order
				break
			}
			e.trade(b, o, maker, lvl.price, min(o.leaves(), maker.leaves()))
		}
		if o.leaves() > 0 && !stp {
			b.rest(o)
		}
	}

	ev := Event{
		Kind: EvReplaced, OrderID: o.id, UserID: o.user, Side: o.side, Type: Limit,
		Price: o.price, Qty: o.qty, FilledQty: o.filled,
		PrevPrice: prevPrice, PrevQty: prevQty, PrevFilled: prevFilled,
	}
	switch {
	case stp:
		ev.Status, ev.Reason = StatusCancelled, ReasonSelfTrade
	case o.leaves() == 0:
		ev.Status = StatusFilled
	default:
		ev.LeavesQty, ev.Resting = o.leaves(), true
		ev.Status = StatusAccepted
		if o.filled > 0 {
			ev.Status = StatusPartiallyFilled
		}
	}
	e.emit(b, ev)
}

// crossesLimit reports whether an order on side with the given limit price may
// trade against a resting order at restingPx.
func crossesLimit(side Side, limit, restingPx int64) bool {
	if side == Buy {
		return restingPx <= limit
	}
	return restingPx >= limit
}

// emitOrder emits an execution report for the incoming order of command c.
func (e *Engine) emitOrder(b *Book, c *Command, st OrderStatus, r Reason, filled int64, resting bool) {
	leaves := int64(0)
	if resting {
		leaves = c.Qty - filled
	}
	e.emit(b, Event{
		Kind: EvOrder, OrderID: c.OrderID, UserID: c.UserID, Side: c.Side, Type: c.Type,
		Price: c.Price, StopPrice: c.StopPrice, Qty: c.Qty, FilledQty: filled, LeavesQty: leaves, Status: st, Reason: r,
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
