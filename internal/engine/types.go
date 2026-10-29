// Package engine implements a deterministic, single-threaded limit order
// book and matching engine with price-time priority (SDD section 5).
//
// The engine is a pure state machine: Process(cmd) mutates in-memory state
// and returns the resulting events. It never reads the wall clock or a
// random source. Timestamps come from the command, and event IDs are
// derived from the command ID, so replaying the same command stream always
// produces byte-identical output.
//
// An Engine is not safe for concurrent use. The matching service runs one
// Engine per Kafka partition, so each book has exactly one writer.
package engine

import (
	"github.com/google/uuid"
)

// Side of an order.
type Side uint8

const (
	Buy  Side = 1
	Sell Side = 2
)

func (s Side) String() string {
	switch s {
	case Buy:
		return "BUY"
	case Sell:
		return "SELL"
	}
	return "UNKNOWN"
}

// Opposite returns the other side.
func (s Side) Opposite() Side {
	if s == Buy {
		return Sell
	}
	return Buy
}

// OrderType is LIMIT or MARKET.
type OrderType uint8

const (
	Limit  OrderType = 1
	Market OrderType = 2
)

func (t OrderType) String() string {
	switch t {
	case Limit:
		return "LIMIT"
	case Market:
		return "MARKET"
	}
	return "UNKNOWN"
}

// CommandKind distinguishes place, cancel and replace.
type CommandKind uint8

const (
	CmdPlace  CommandKind = 1
	CmdCancel CommandKind = 2
	// CmdReplace amends the price and/or total quantity of a resting LIMIT
	// order (cancel-replace). Command.Price is the new limit price and
	// Command.Qty the new TOTAL order quantity (filled + open), so the new
	// open quantity is Qty - filled.
	//
	//   - same price, lower qty: amended in place, time priority is kept;
	//   - same price, higher qty: loses time priority (re-queued at the tail);
	//   - different price: atomic cancel + re-insert; the order loses time
	//     priority and, if the new price crosses the book, trades as a taker.
	CmdReplace CommandKind = 3
)

// Command is an input to the engine.
type Command struct {
	// ID uniquely identifies this command. Output event IDs are derived
	// from it, so it must be stable across redeliveries.
	ID      uuid.UUID
	Kind    CommandKind
	Symbol  string
	OrderID uuid.UUID
	UserID  uuid.UUID
	Side    Side
	Type    OrderType
	// Price is the limit price for LIMIT orders. For MARKET orders it is
	// the optional protection price: the engine never fills a market buy
	// above it or a market sell below it (0 = unbounded).
	Price int64
	Qty   int64
	// Timestamp in unix nanoseconds, assigned upstream (order-service).
	Timestamp int64
}

// OrderStatus mirrors the orders.status column.
type OrderStatus uint8

const (
	StatusAccepted        OrderStatus = 1
	StatusPartiallyFilled OrderStatus = 2
	StatusFilled          OrderStatus = 3
	StatusCancelled       OrderStatus = 4
	StatusRejected        OrderStatus = 5
)

func (s OrderStatus) String() string {
	return [...]string{"UNKNOWN", "ACCEPTED", "PARTIALLY_FILLED", "FILLED", "CANCELLED", "REJECTED"}[s]
}

// Terminal reports whether no further events will be emitted for the order.
func (s OrderStatus) Terminal() bool {
	return s == StatusFilled || s == StatusCancelled || s == StatusRejected
}

// Reason explains a rejection or cancellation.
type Reason uint8

const (
	ReasonNone           Reason = 0
	ReasonUserCancel     Reason = 1 // cancel command
	ReasonIOCRemainder   Reason = 2 // market order: unfilled remainder
	ReasonNoLiquidity    Reason = 3 // market order: nothing to match
	ReasonSelfTrade      Reason = 4 // self-trade prevention (cancel newest)
	ReasonInvalid        Reason = 5 // malformed command
	ReasonDuplicateOrder Reason = 6 // order id already seen
	ReasonUnknownOrder   Reason = 7 // cancel: order not on the book
	ReasonPriceBand      Reason = 8 // market order: remainder beyond protection price
	ReasonQtyTooLow      Reason = 9 // replace: new total qty <= already filled qty
	ReasonNoChange       Reason = 10
)

func (r Reason) String() string {
	return [...]string{"", "USER_CANCEL", "IOC_REMAINDER", "NO_LIQUIDITY", "SELF_TRADE",
		"INVALID", "DUPLICATE_ORDER", "UNKNOWN_ORDER", "PRICE_BAND",
		"QTY_TOO_LOW", "NO_CHANGE"}[r]
}

// EventKind distinguishes event payloads.
type EventKind uint8

const (
	// EvOrder is an execution report: the new state of one order.
	EvOrder EventKind = 1
	// EvTrade is a match between a resting (maker) and incoming (taker) order.
	EvTrade EventKind = 2
	// EvCancelReject means a cancel command could not be applied.
	EvCancelReject EventKind = 3
	// EvDuplicate means a place command was ignored because its order id was
	// already processed (e.g. an outbox re-publish). It carries no state
	// change and consumers can ignore it.
	EvDuplicate EventKind = 4
	// EvReplaced is the final execution report of an accepted replace. It
	// carries the order's state after the replace (and after any trades the
	// replace triggered) plus the PrevPrice/PrevQty/PrevFilled it replaced.
	EvReplaced EventKind = 5
	// EvReplaceReject means a replace command could not be applied; the
	// order is untouched. Reason says why.
	EvReplaceReject EventKind = 6
)

// Event is an output of the engine.
type Event struct {
	ID        uuid.UUID // deterministic: f(command ID, index)
	CommandID uuid.UUID
	Kind      EventKind
	Symbol    string
	Seq       uint64 // per-symbol sequence number, gap-free
	Timestamp int64

	// EvOrder / EvCancelReject / EvDuplicate
	OrderID   uuid.UUID
	UserID    uuid.UUID
	Side      Side
	Type      OrderType
	Price     int64
	Qty       int64
	FilledQty int64
	LeavesQty int64 // qty still open (on the book if Resting)
	Status    OrderStatus
	Reason    Reason
	// Resting is true when LeavesQty is on the book after this event.
	Resting bool

	// EvReplaced: the order's state before the replace. PrevFilled is the
	// filled quantity at the moment of the replace, before any trade the
	// replace itself triggered. Together with Price/Qty they let a consumer
	// compute the change in reservation without knowing anything else about
	// the engine's state.
	PrevPrice  int64
	PrevQty    int64
	PrevFilled int64

	// EvTrade
	Trade Trade
}

// Trade describes one execution. MatchID equals the event ID.
type Trade struct {
	MatchID     uuid.UUID
	Price       int64
	Qty         int64
	BuyOrderID  uuid.UUID
	SellOrderID uuid.UUID
	BuyerID     uuid.UUID
	SellerID    uuid.UUID
	MakerOrder  uuid.UUID
	Aggressor   Side

	// BuyLimit is the limit (or market protection) price the buy order held
	// when this trade executed, 0 if unbounded. Settlement uses it as the
	// per-share reservation so it does not depend on whether a concurrent
	// replace of the buy order has already been applied downstream.
	BuyLimit int64
}

// Level is an aggregated price level used for depth snapshots.
type Level struct {
	Price  int64 `json:"price"`
	Qty    int64 `json:"qty"`
	Orders int   `json:"orders"`
}
