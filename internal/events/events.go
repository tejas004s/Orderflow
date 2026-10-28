// Package events defines the Kafka event envelope and wire payloads specified
// in SDD section 4.3, along with conversion helpers to and from internal/engine.
package events

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/tejas004s/orderflow/internal/engine"
)

// SchemaVersion is carried on every Envelope from day one (SDD section 4.3).
const SchemaVersion uint32 = 1

// Event type identifiers.
const (
	TypeOrderCommand  = "order.command"
	TypeOrderEvent    = "order.event"
	TypeTradeExecuted = "trade.executed"
	TypeMarketTick    = "market.tick"
	TypeCandle1m      = "market.candle.1m"
)

// Envelope wraps every Kafka message with metadata (SDD section 4.3).
type Envelope struct {
	EventID       uuid.UUID       `json:"event_id"`
	EventType     string          `json:"event_type"`
	SchemaVersion uint32          `json:"schema_version"`
	OccurredAt    int64           `json:"occurred_at"` // unix nanoseconds
	Producer      string          `json:"producer"`
	CorrelationID string          `json:"correlation_id,omitempty"`
	Payload       json.RawMessage `json:"payload"`
}

// Wrap constructs and marshals an Envelope around payload v.
func Wrap(eventID uuid.UUID, eventType, producer, correlationID string, occurredAt int64, v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal payload: %w", err)
	}
	if occurredAt == 0 {
		occurredAt = time.Now().UnixNano()
	}
	env := Envelope{
		EventID:       eventID,
		EventType:     eventType,
		SchemaVersion: SchemaVersion,
		OccurredAt:    occurredAt,
		Producer:      producer,
		CorrelationID: correlationID,
		Payload:       raw,
	}
	return json.Marshal(env)
}

// Unwrap decodes an Envelope and optionally unmarshals its payload into dst.
func Unwrap(data []byte, dst any) (Envelope, error) {
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return Envelope{}, fmt.Errorf("unmarshal envelope: %w", err)
	}
	if env.SchemaVersion == 0 {
		return Envelope{}, fmt.Errorf("missing schema_version")
	}
	if dst != nil {
		if err := json.Unmarshal(env.Payload, dst); err != nil {
			return env, fmt.Errorf("unmarshal %s payload: %w", env.EventType, err)
		}
	}
	return env, nil
}

// OrderCommand is the payload on topic orders.commands.
type OrderCommand struct {
	CommandID uuid.UUID `json:"command_id"`
	Kind      string    `json:"kind"` // "PLACE" | "CANCEL"
	Symbol    string    `json:"symbol"`
	OrderID   uuid.UUID `json:"order_id"`
	UserID    uuid.UUID `json:"user_id"`
	Side      string    `json:"side,omitempty"`       // "BUY" | "SELL"
	OrderType string    `json:"order_type,omitempty"` // "LIMIT" | "MARKET"
	Price     int64     `json:"price,omitempty"`
	Qty       int64     `json:"qty,omitempty"`
	Timestamp int64     `json:"timestamp"`
}

// ToEngine converts wire OrderCommand into engine.Command.
func (c OrderCommand) ToEngine() (engine.Command, error) {
	out := engine.Command{
		ID:        c.CommandID,
		Symbol:    c.Symbol,
		OrderID:   c.OrderID,
		UserID:    c.UserID,
		Price:     c.Price,
		Qty:       c.Qty,
		Timestamp: c.Timestamp,
	}
	switch c.Kind {
	case "PLACE":
		out.Kind = engine.CmdPlace
	case "CANCEL":
		out.Kind = engine.CmdCancel
		return out, nil
	default:
		return out, fmt.Errorf("unknown command kind %q", c.Kind)
	}
	switch c.Side {
	case "BUY":
		out.Side = engine.Buy
	case "SELL":
		out.Side = engine.Sell
	default:
		return out, fmt.Errorf("unknown side %q", c.Side)
	}
	switch c.OrderType {
	case "LIMIT":
		out.Type = engine.Limit
	case "MARKET":
		out.Type = engine.Market
	default:
		return out, fmt.Errorf("unknown order_type %q", c.OrderType)
	}
	return out, nil
}

// FromEngineCommand converts engine.Command to wire OrderCommand.
func FromEngineCommand(c engine.Command) OrderCommand {
	kind := "PLACE"
	if c.Kind == engine.CmdCancel {
		kind = "CANCEL"
	}
	return OrderCommand{
		CommandID: c.ID,
		Kind:      kind,
		Symbol:    c.Symbol,
		OrderID:   c.OrderID,
		UserID:    c.UserID,
		Side:      c.Side.String(),
		OrderType: c.Type.String(),
		Price:     c.Price,
		Qty:       c.Qty,
		Timestamp: c.Timestamp,
	}
}

// OrderEvent is the payload on topic orders.events.
type OrderEvent struct {
	EventID   uuid.UUID `json:"event_id"`
	CommandID uuid.UUID `json:"command_id"`
	Kind      string    `json:"kind"` // "ORDER" | "CANCEL_REJECT" | "DUPLICATE"
	Symbol    string    `json:"symbol"`
	Seq       uint64    `json:"seq"`
	Timestamp int64     `json:"timestamp"`
	OrderID   uuid.UUID `json:"order_id"`
	UserID    uuid.UUID `json:"user_id"`
	Side      string    `json:"side,omitempty"`
	OrderType string    `json:"order_type,omitempty"`
	Price     int64     `json:"price,omitempty"`
	Qty       int64     `json:"qty,omitempty"`
	FilledQty int64     `json:"filled_qty"`
	LeavesQty int64     `json:"leaves_qty"`
	Status    string    `json:"status,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	Resting   bool      `json:"resting"`
}

// TradeExecuted is the payload on topic trades.executed.
type TradeExecuted struct {
	MatchID       uuid.UUID `json:"match_id"`
	CommandID     uuid.UUID `json:"command_id"`
	Symbol        string    `json:"symbol"`
	Seq           uint64    `json:"seq"`
	Price         int64     `json:"price"`
	Qty           int64     `json:"qty"`
	BuyOrderID    uuid.UUID `json:"buy_order_id"`
	SellOrderID   uuid.UUID `json:"sell_order_id"`
	BuyerID       uuid.UUID `json:"buyer_id"`
	SellerID      uuid.UUID `json:"seller_id"`
	MakerOrderID  uuid.UUID `json:"maker_order_id"`
	AggressorSide string    `json:"aggressor_side"`
	ExecutedAt    int64     `json:"executed_at"`
}

// FromEngineEvent converts an engine.Event to either an OrderEvent or a TradeExecuted.
func FromEngineEvent(ev engine.Event) (*OrderEvent, *TradeExecuted) {
	if ev.Kind == engine.EvTrade {
		tr := ev.Trade
		return nil, &TradeExecuted{
			MatchID:       tr.MatchID,
			CommandID:     ev.CommandID,
			Symbol:        ev.Symbol,
			Seq:           ev.Seq,
			Price:         tr.Price,
			Qty:           tr.Qty,
			BuyOrderID:    tr.BuyOrderID,
			SellOrderID:   tr.SellOrderID,
			BuyerID:       tr.BuyerID,
			SellerID:      tr.SellerID,
			MakerOrderID:  tr.MakerOrder,
			AggressorSide: tr.Aggressor.String(),
			ExecutedAt:    ev.Timestamp,
		}
	}
	kind := "ORDER"
	switch ev.Kind {
	case engine.EvCancelReject:
		kind = "CANCEL_REJECT"
	case engine.EvDuplicate:
		kind = "DUPLICATE"
	}
	return &OrderEvent{
		EventID:   ev.ID,
		CommandID: ev.CommandID,
		Kind:      kind,
		Symbol:    ev.Symbol,
		Seq:       ev.Seq,
		Timestamp: ev.Timestamp,
		OrderID:   ev.OrderID,
		UserID:    ev.UserID,
		Side:      ev.Side.String(),
		OrderType: ev.Type.String(),
		Price:     ev.Price,
		Qty:       ev.Qty,
		FilledQty: ev.FilledQty,
		LeavesQty: ev.LeavesQty,
		Status:    ev.Status.String(),
		Reason:    ev.Reason.String(),
		Resting:   ev.Resting,
	}, nil
}

// MarketTick is the payload on topic market.ticks.
type MarketTick struct {
	Symbol    string `json:"symbol"`
	Price     int64  `json:"price"`
	Volume    int64  `json:"volume"`
	Timestamp int64  `json:"timestamp"`
}

// Candle is the payload on topic market.candles.1m.
type Candle struct {
	Symbol      string `json:"symbol"`
	Interval    string `json:"interval"`
	BucketStart int64  `json:"bucket_start"` // unix seconds
	Open        int64  `json:"open"`
	High        int64  `json:"high"`
	Low         int64  `json:"low"`
	Close       int64  `json:"close"`
	Volume      int64  `json:"volume"`
}

