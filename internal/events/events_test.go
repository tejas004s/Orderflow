package events

import (
	"testing"

	"github.com/google/uuid"

	"github.com/tejas004s/orderflow/internal/engine"
)

func TestEnvelopeAndCommandRoundTrip(t *testing.T) {
	cmd := engine.Command{
		ID:        uuid.New(),
		Kind:      engine.CmdPlace,
		Symbol:    "AAPL",
		OrderID:   uuid.New(),
		UserID:    uuid.New(),
		Side:      engine.Buy,
		Type:      engine.Limit,
		Price:     18900,
		Qty:       25,
		Timestamp: 1700000000000000000,
	}
	wire := FromEngineCommand(cmd)
	raw, err := Wrap(cmd.ID, TypeOrderCommand, "order-service", "corr-123", cmd.Timestamp, wire)
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}

	var decoded OrderCommand
	env, err := Unwrap(raw, &decoded)
	if err != nil {
		t.Fatalf("unwrap: %v", err)
	}
	if env.SchemaVersion != SchemaVersion || env.CorrelationID != "corr-123" || env.Producer != "order-service" {
		t.Fatalf("unexpected envelope: %+v", env)
	}
	back, err := decoded.ToEngine()
	if err != nil {
		t.Fatalf("toEngine: %v", err)
	}
	if back != cmd {
		t.Fatalf("round-trip mismatch: got %+v want %+v", back, cmd)
	}
}

func TestFromEngineEventConversion(t *testing.T) {
	e := engine.New()
	evs := e.Process(engine.Command{
		ID: uuid.New(), Kind: engine.CmdPlace, Symbol: "AAPL",
		OrderID: uuid.New(), UserID: uuid.New(), Side: engine.Sell, Type: engine.Limit,
		Price: 10000, Qty: 10, Timestamp: 100,
	})
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evs))
	}
	oev, tev := FromEngineEvent(evs[0])
	if oev == nil || tev != nil || oev.Status != "ACCEPTED" {
		t.Fatalf("unexpected conversion: oev=%+v tev=%+v", oev, tev)
	}

	evs2 := e.Process(engine.Command{
		ID: uuid.New(), Kind: engine.CmdPlace, Symbol: "AAPL",
		OrderID: uuid.New(), UserID: uuid.New(), Side: engine.Buy, Type: engine.Limit,
		Price: 10000, Qty: 10, Timestamp: 200,
	})
	var sawTrade bool
	for _, ev := range evs2 {
		_, tr := FromEngineEvent(ev)
		if tr != nil {
			sawTrade = true
			if tr.Price != 10000 || tr.Qty != 10 || tr.AggressorSide != "BUY" {
				t.Fatalf("unexpected trade: %+v", tr)
			}
		}
	}
	if !sawTrade {
		t.Fatal("expected TradeExecuted")
	}
}

