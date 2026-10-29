package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/tejas004s/orderflow/internal/auth"
	"github.com/tejas004s/orderflow/internal/events"
	"github.com/tejas004s/orderflow/internal/gateway"
	"github.com/tejas004s/orderflow/internal/gateway/ws"
	"github.com/tejas004s/orderflow/internal/kafka"
	"github.com/tejas004s/orderflow/internal/ledger"
	"github.com/tejas004s/orderflow/internal/marketdata"
	"github.com/tejas004s/orderflow/internal/matching"
	"github.com/tejas004s/orderflow/internal/order"
	"github.com/tejas004s/orderflow/internal/outbox"
	"github.com/tejas004s/orderflow/internal/testpg"
)

type memPublisher struct {
	msgs []outbox.Message
}

func (p *memPublisher) Publish(_ context.Context, msgs []outbox.Message) error {
	p.msgs = append(p.msgs, msgs...)
	return nil
}

func (p *memPublisher) take() []outbox.Message {
	out := p.msgs
	p.msgs = nil
	return out
}

func TestGatewayAuthRefreshReuseRESTAndWebSocket(t *testing.T) {
	pool, _ := testpg.New(t)
	ctx := context.Background()
	silentLog := slog.New(slog.NewTextHandler(io.Discard, nil))

	authSvc := auth.NewService(pool, "test-jwt-secret-key")
	hub := ws.NewHub(authSvc, silentLog)

	cmdBus := &memPublisher{}
	relay := outbox.NewRelay(pool, cmdBus, silentLog)
	ordSvc := order.NewService(pool, relay)

	gw := gateway.NewServer(pool, authSvc, hub, "", silentLog)
	gw.LocalOrders = ordSvc

	srv := httptest.NewServer(gw.Router(nil))
	defer srv.Close()

	// 1. Register Alice and Bob via POST /api/v1/auth/register
	register := func(email string) auth.TokenPair {
		raw, _ := json.Marshal(map[string]string{"email": email, "password": "password123"})
		resp, err := http.Post(srv.URL+"/api/v1/auth/register", "application/json", bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("register %s: %v", email, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("register status %d: %s", resp.StatusCode, b)
		}
		var pair auth.TokenPair
		_ = json.NewDecoder(resp.Body).Decode(&pair)
		return pair
	}
	aliceTok := register("alice@example.com")
	bobTok := register("bob@example.com")

	// 2. Test refresh token rotation and reuse detection (SDD section 13).
	refreshCall := func(rt string) (*http.Response, auth.TokenPair) {
		raw, _ := json.Marshal(map[string]string{"refresh_token": rt})
		resp, err := http.Post(srv.URL+"/api/v1/auth/refresh", "application/json", bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var pair auth.TokenPair
		_ = json.NewDecoder(resp.Body).Decode(&pair)
		return resp, pair
	}
	respRot, rotated := refreshCall(aliceTok.RefreshToken)
	if respRot.StatusCode != http.StatusOK || rotated.AccessToken == "" {
		t.Fatalf("expected 200 on refresh rotation, got %d", respRot.StatusCode)
	}
	// Replaying the old refresh token must fail AND revoke the rotated token family!
	respReplayOld, _ := refreshCall(aliceTok.RefreshToken)
	if respReplayOld.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 on refresh replay, got %d", respReplayOld.StatusCode)
	}
	respAfterRevoke, _ := refreshCall(rotated.RefreshToken)
	if respAfterRevoke.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected family revocation to reject rotated token, got %d", respAfterRevoke.StatusCode)
	}

	// 3. Connect Alice over WebSocket and subscribe to book:AAPL, trades:AAPL, user.orders.
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws?token=" + aliceTok.AccessToken
	wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
	defer wcancel()
	conn, _, err := websocket.Dial(wctx, wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer conn.CloseNow()

	subMsg, _ := json.Marshal(ws.InboundMsg{
		Op:       "subscribe",
		Channels: []string{"book:AAPL", "trades:AAPL", "user.orders"},
	})
	if err := conn.Write(wctx, websocket.MessageText, subMsg); err != nil {
		t.Fatalf("ws write subscribe: %v", err)
	}

	readWS := func() ws.OutboundMsg {
		_, data, err := conn.Read(wctx)
		if err != nil {
			t.Fatalf("ws read: %v", err)
		}
		var m ws.OutboundMsg
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("ws decode: %v", err)
		}
		return m
	}

	// First message on subscribing to book:AAPL must be book.snapshot.
	snapMsg := readWS()
	if snapMsg.Type != "book.snapshot" || snapMsg.Channel != "book:AAPL" {
		t.Fatalf("expected book.snapshot, got %+v", snapMsg)
	}

	// 4. Seed Bob with 50 AAPL shares, place Bob's sell order and Alice's buy order.
	authReq := func(method, path, token, idem string, body any) (*http.Response, []byte) {
		var rdr io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rdr = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, srv.URL+path, rdr)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		if idem != "" {
			req.Header.Set("Idempotency-Key", idem)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return resp, out
	}

	resp, _ := authReq(http.MethodPost, "/api/v1/portfolio/seed", bobTok.AccessToken, "", map[string]any{
		"symbol": "AAPL", "shares": 50,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("seed status %d", resp.StatusCode)
	}

	resp, body := authReq(http.MethodPost, "/api/v1/orders", bobTok.AccessToken, "bob-s1", map[string]any{
		"symbol": "AAPL", "side": "SELL", "type": "LIMIT", "price": 18900, "qty": 10,
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("bob order %d: %s", resp.StatusCode, body)
	}

	resp, body = authReq(http.MethodPost, "/api/v1/orders", aliceTok.AccessToken, "alice-b1", map[string]any{
		"symbol": "AAPL", "side": "BUY", "type": "LIMIT", "price": 18900, "qty": 6,
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("alice order %d: %s", resp.StatusCode, body)
	}

	// Run outbox -> matching engine -> ledger + gateway fanout + marketdata.
	engBus := &memPublisher{}
	proc := matching.NewProcessor(engBus, nil, silentLog)
	ledgerSvc := ledger.NewService(pool, nil, silentLog)
	mdSvc := marketdata.NewService(pool, nil, silentLog)
	if err := mdSvc.Init(ctx); err != nil {
		t.Fatalf("md init: %v", err)
	}

	if _, err := relay.DrainBatch(ctx); err != nil {
		t.Fatal(err)
	}
	for i, m := range cmdBus.take() {
		if err := proc.HandleRaw(ctx, 0, int64(i), m.Key, m.Payload); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range engBus.take() {
		if err := ledgerSvc.HandleRaw(ctx, m.Topic, m.Key, m.Payload); err != nil {
			t.Fatal(err)
		}
		if m.Topic == kafka.TopicOrderEvents {
			var oev events.OrderEvent
			_, _ = events.Unwrap(m.Payload, &oev)
			hub.OnOrderEvent(oev)
		} else if m.Topic == kafka.TopicTrades {
			var tr events.TradeExecuted
			_, _ = events.Unwrap(m.Payload, &tr)
			hub.OnTrade(tr)
			_ = mdSvc.OnTrade(ctx, tr)
		}
	}

	// 5. Read WebSocket messages: Alice must receive book.delta, trade, and order.update.
	var sawDelta, sawTrade, sawOrderUpdate bool
	for !(sawDelta && sawTrade && sawOrderUpdate) {
		m := readWS()
		switch m.Type {
		case "book.delta":
			sawDelta = true
		case "trade":
			sawTrade = true
		case "order.update":
			sawOrderUpdate = true
		}
	}

	// 6. Verify REST endpoints: /book/AAPL, /portfolio, /trades, /candles/AAPL.
	resp, body = authReq(http.MethodGet, "/api/v1/book/AAPL?depth=5", aliceTok.AccessToken, "", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"qty":4`) {
		t.Fatalf("expected 4 resting ask shares on book, got %d: %s", resp.StatusCode, body)
	}

	resp, body = authReq(http.MethodGet, "/api/v1/portfolio", aliceTok.AccessToken, "", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"symbol":"AAPL"`) {
		t.Fatalf("portfolio %d: %s", resp.StatusCode, body)
	}

	resp, body = authReq(http.MethodGet, "/api/v1/trades?symbol=AAPL", aliceTok.AccessToken, "", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"price":18900`) {
		t.Fatalf("trades %d: %s", resp.StatusCode, body)
	}

	resp, body = authReq(http.MethodGet, "/api/v1/candles/AAPL?interval=1m&limit=10", aliceTok.AccessToken, "", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"candles"`) {
		t.Fatalf("candles %d: %s", resp.StatusCode, body)
	}
}
