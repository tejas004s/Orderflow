// Package ws implements the api-gateway WebSocket hub, per-symbol order book
// projection, snapshot-then-delta protocol with sequence numbers, and bounded
// per-client backpressure (SDD section 8.2).
package ws

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/tejas004s/orderflow/internal/auth"
	"github.com/tejas004s/orderflow/internal/engine"
	"github.com/tejas004s/orderflow/internal/events"
	"github.com/tejas004s/orderflow/internal/obs"
)

// ClientBufferSize is the bounded per-client outbound message capacity. Slow
// clients whose buffer fills up are dropped so they never block the hub.
const ClientBufferSize = 256

// InboundMsg is sent by WebSocket clients (`{"op":"subscribe","channels":[...]}`).
type InboundMsg struct {
	Op       string   `json:"op"`
	Channels []string `json:"channels"`
}

// OutboundMsg is pushed by the server over WebSocket.
type OutboundMsg struct {
	Type    string `json:"type"`              // "book.snapshot" | "book.delta" | "trade" | "candle" | "order.update"
	Channel string `json:"channel,omitempty"` // e.g. "book:AAPL"
	Symbol  string `json:"symbol,omitempty"`
	Seq     uint64 `json:"seq,omitempty"`
	PrevSeq uint64 `json:"prev_seq,omitempty"`
	Data    any    `json:"data"`
}

// BookState is the payload inside book.snapshot and book.delta messages.
type BookState struct {
	Symbol string         `json:"symbol"`
	Seq    uint64         `json:"seq"`
	Bids   []engine.Level `json:"bids"`
	Asks   []engine.Level `json:"asks"`
}

// restingOrder tracks a resting order in the gateway's per-symbol book copy.
type restingOrder struct {
	side   string
	price  int64
	leaves int64
}

type symbolBook struct {
	symbol string
	seq    uint64
	orders map[uuid.UUID]restingOrder
	bids   map[int64]int64 // price -> total qty
	bidCnt map[int64]int   // price -> order count
	asks   map[int64]int64
	askCnt map[int64]int
}

func newSymbolBook(sym string) *symbolBook {
	return &symbolBook{
		symbol: sym,
		orders: map[uuid.UUID]restingOrder{},
		bids:   map[int64]int64{},
		bidCnt: map[int64]int{},
		asks:   map[int64]int64{},
		askCnt: map[int64]int{},
	}
}

func (b *symbolBook) applyOrderEvent(ev events.OrderEvent) {
	if ev.Seq > b.seq {
		b.seq = ev.Seq
	}
	if prev, ok := b.orders[ev.OrderID]; ok {
		b.addLevel(prev.side, prev.price, -prev.leaves, -1)
		delete(b.orders, ev.OrderID)
	}
	if ev.Resting && ev.LeavesQty > 0 && (ev.Status == "ACCEPTED" || ev.Status == "PARTIALLY_FILLED") {
		b.orders[ev.OrderID] = restingOrder{side: ev.Side, price: ev.Price, leaves: ev.LeavesQty}
		b.addLevel(ev.Side, ev.Price, ev.LeavesQty, 1)
	}
}

func (b *symbolBook) addLevel(side string, price, deltaQty int64, deltaCnt int) {
	if price <= 0 {
		return
	}
	if side == "BUY" {
		b.bids[price] += deltaQty
		b.bidCnt[price] += deltaCnt
		if b.bids[price] <= 0 || b.bidCnt[price] <= 0 {
			delete(b.bids, price)
			delete(b.bidCnt, price)
		}
	} else if side == "SELL" {
		b.asks[price] += deltaQty
		b.askCnt[price] += deltaCnt
		if b.asks[price] <= 0 || b.askCnt[price] <= 0 {
			delete(b.asks, price)
			delete(b.askCnt, price)
		}
	}
}

func (b *symbolBook) snapshot(depth int) BookState {
	if depth <= 0 || depth > 50 {
		depth = 20
	}
	bids := make([]engine.Level, 0, len(b.bids))
	for px, q := range b.bids {
		bids = append(bids, engine.Level{Price: px, Qty: q, Orders: b.bidCnt[px]})
	}
	sort.Slice(bids, func(i, j int) bool { return bids[i].Price > bids[j].Price })
	if len(bids) > depth {
		bids = bids[:depth]
	}

	asks := make([]engine.Level, 0, len(b.asks))
	for px, q := range b.asks {
		asks = append(asks, engine.Level{Price: px, Qty: q, Orders: b.askCnt[px]})
	}
	sort.Slice(asks, func(i, j int) bool { return asks[i].Price < asks[j].Price })
	if len(asks) > depth {
		asks = asks[:depth]
	}

	return BookState{Symbol: b.symbol, Seq: b.seq, Bids: bids, Asks: asks}
}

type client struct {
	userID uuid.UUID
	send   chan []byte
	subs   map[string]struct{}
	closed bool
}

// Hub manages connected WebSocket clients and per-symbol book projections.
type Hub struct {
	Auth *auth.Service
	Log  *slog.Logger

	mu      sync.RWMutex
	clients map[*client]struct{}
	books   map[string]*symbolBook
	lastPx  map[string]int64
}

// NewHub creates a WebSocket Hub.
func NewHub(authSvc *auth.Service, log *slog.Logger) *Hub {
	return &Hub{
		Auth:    authSvc,
		Log:     log,
		clients: map[*client]struct{}{},
		books:   map[string]*symbolBook{},
		lastPx:  map[string]int64{},
	}
}

// ClientCount returns the number of active WebSocket connections.
func (h *Hub) ClientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// BookSnapshot returns the gateway's current book projection for symbol.
func (h *Hub) BookSnapshot(symbol string, depth int) BookState {
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	h.mu.RLock()
	defer h.mu.RUnlock()
	bk, ok := h.books[symbol]
	if !ok {
		return BookState{Symbol: symbol, Bids: []engine.Level{}, Asks: []engine.Level{}}
	}
	return bk.snapshot(depth)
}

// LastPrice returns the latest observed trade or tick price for symbol, or 0.
func (h *Hub) LastPrice(symbol string) int64 {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.lastPx[strings.ToUpper(symbol)]
}

// OnOrderEvent updates the per-symbol book projection and broadcasts both
// book.delta (to subscribers of book:<SYM>) and order.update (to the order's
// owner if subscribed to user.orders).
func (h *Hub) OnOrderEvent(ev events.OrderEvent) {
	if ev.Kind == "DUPLICATE" {
		return
	}
	h.mu.Lock()
	bk := h.books[ev.Symbol]
	if bk == nil {
		bk = newSymbolBook(ev.Symbol)
		h.books[ev.Symbol] = bk
	}
	prevSeq := bk.seq
	bk.applyOrderEvent(ev)
	snap := bk.snapshot(20)
	h.mu.Unlock()

	bookCh := "book:" + ev.Symbol
	h.broadcastChannel(bookCh, OutboundMsg{
		Type:    "book.delta",
		Channel: bookCh,
		Symbol:  ev.Symbol,
		Seq:     snap.Seq,
		PrevSeq: prevSeq,
		Data:    snap,
	})

	if ev.UserID != uuid.Nil {
		h.broadcastUser(ev.UserID, OutboundMsg{
			Type:    "order.update",
			Channel: "user.orders",
			Symbol:  ev.Symbol,
			Seq:     ev.Seq,
			Data:    ev,
		})
	}
}

// OnTrade updates the last price and broadcasts to trades:<SYM>.
func (h *Hub) OnTrade(tr events.TradeExecuted) {
	h.mu.Lock()
	h.lastPx[tr.Symbol] = tr.Price
	if bk := h.books[tr.Symbol]; bk != nil && tr.Seq > bk.seq {
		bk.seq = tr.Seq
	}
	h.mu.Unlock()

	ch := "trades:" + tr.Symbol
	h.broadcastChannel(ch, OutboundMsg{
		Type:    "trade",
		Channel: ch,
		Symbol:  tr.Symbol,
		Seq:     tr.Seq,
		Data:    tr,
	})
}

// OnTick records the simulated price and broadcasts to ticks:<SYM>.
func (h *Hub) OnTick(tk events.MarketTick) {
	h.mu.Lock()
	if _, has := h.lastPx[tk.Symbol]; !has || tk.Price > 0 {
		h.lastPx[tk.Symbol] = tk.Price
	}
	h.mu.Unlock()

	ch := "ticks:" + tk.Symbol
	h.broadcastChannel(ch, OutboundMsg{
		Type:    "tick",
		Channel: ch,
		Symbol:  tk.Symbol,
		Data:    tk,
	})
}

// OnCandle broadcasts an OHLCV candle update to candles:<SYM>.
func (h *Hub) OnCandle(cd events.Candle) {
	ch := "candles:" + cd.Symbol
	h.broadcastChannel(ch, OutboundMsg{
		Type:    "candle",
		Channel: ch,
		Symbol:  cd.Symbol,
		Data:    cd,
	})
}

func (h *Hub) broadcastChannel(ch string, msg OutboundMsg) {
	raw, err := json.Marshal(msg)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if _, ok := c.subs[ch]; !ok {
			continue
		}
		h.enqueueLocked(c, raw)
	}
}

func (h *Hub) broadcastUser(uid uuid.UUID, msg OutboundMsg) {
	raw, err := json.Marshal(msg)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if c.userID != uid {
			continue
		}
		if _, ok := c.subs["user.orders"]; !ok {
			continue
		}
		h.enqueueLocked(c, raw)
	}
}

func (h *Hub) enqueueLocked(c *client, raw []byte) {
	if c.closed {
		return
	}
	select {
	case c.send <- raw:
	default:
		// Slow client: drop rather than block the hub (SDD section 8.2).
		c.closed = true
		close(c.send)
		delete(h.clients, c)
	}
}

// ServeWS upgrades an HTTP request to WebSocket and manages its lifecycle.
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request) {
	var uid uuid.UUID
	if h.Auth != nil {
		tok := r.URL.Query().Get("token")
		if tok == "" && strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			tok = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		if tok != "" {
			if parsed, _, err := h.Auth.VerifyAccessToken(tok); err == nil {
				uid = parsed
			}
		}
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // allow cross-origin dev UI
	})
	if err != nil {
		return
	}
	defer conn.CloseNow()

	c := &client{
		userID: uid,
		send:   make(chan []byte, ClientBufferSize),
		subs:   map[string]struct{}{},
	}
	h.mu.Lock()
	h.clients[c] = struct{}{}
	obs.WSConnections.Set(float64(len(h.clients)))
	h.mu.Unlock()

	defer func() {
		h.mu.Lock()
		if !c.closed {
			c.closed = true
			close(c.send)
		}
		delete(h.clients, c)
		obs.WSConnections.Set(float64(len(h.clients)))
		h.mu.Unlock()
	}()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Writer goroutine
	go func() {
		defer cancel()
		for msg := range c.send {
			wctx, wcancel := context.WithTimeout(ctx, 5*time.Second)
			err := conn.Write(wctx, websocket.MessageText, msg)
			wcancel()
			if err != nil {
				return
			}
		}
	}()

	// Reader loop
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var in InboundMsg
		if err := json.Unmarshal(data, &in); err != nil {
			continue
		}
		switch strings.ToLower(in.Op) {
		case "subscribe":
			for _, ch := range in.Channels {
				ch = strings.TrimSpace(ch)
				if ch == "" {
					continue
				}
				h.mu.Lock()
				c.subs[ch] = struct{}{}
				h.mu.Unlock()
				if strings.HasPrefix(ch, "book:") {
					sym := strings.ToUpper(strings.TrimPrefix(ch, "book:"))
					snap := h.BookSnapshot(sym, 20)
					raw, _ := json.Marshal(OutboundMsg{
						Type:    "book.snapshot",
						Channel: "book:" + sym,
						Symbol:  sym,
						Seq:     snap.Seq,
						Data:    snap,
					})
					h.mu.Lock()
					h.enqueueLocked(c, raw)
					h.mu.Unlock()
				}
			}
		case "unsubscribe":
			h.mu.Lock()
			for _, ch := range in.Channels {
				delete(c.subs, strings.TrimSpace(ch))
			}
			h.mu.Unlock()
		}
	}
}
