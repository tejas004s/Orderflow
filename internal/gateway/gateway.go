package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/tejas004s/orderflow/internal/auth"
	"github.com/tejas004s/orderflow/internal/events"
	"github.com/tejas004s/orderflow/internal/gateway/ws"
	"github.com/tejas004s/orderflow/internal/httpx"
	"github.com/tejas004s/orderflow/internal/obs"
	"github.com/tejas004s/orderflow/internal/order"
)

// Server bundles all gateway dependencies.
type Server struct {
	Pool            *pgxpool.Pool
	Auth            *auth.Service
	Hub             *ws.Hub
	Limiter         *RateLimiter
	OrderServiceURL string         // if empty, LocalOrders is used directly
	LocalOrders     *order.Service // optional in-process fallback for tests
	Log             *slog.Logger
	HTTPClient      *http.Client
}

// NewServer constructs a gateway Server.
func NewServer(pool *pgxpool.Pool, authSvc *auth.Service, hub *ws.Hub, orderURL string, log *slog.Logger) *Server {
	return &Server{
		Pool:            pool,
		Auth:            authSvc,
		Hub:             hub,
		Limiter:         NewRateLimiter(100, 200), // 100 req/s per user, burst 200
		OrderServiceURL: strings.TrimRight(orderURL, "/"),
		Log:             log,
		HTTPClient:      &http.Client{Timeout: 10 * time.Second},
	}
}

// Router builds the full chi HTTP handler for api-gateway.
func (s *Server) Router(baseMux *http.ServeMux) http.Handler {
	r := chi.NewRouter()
	r.Use(corsMiddleware)
	r.Use(httpx.CorrelationMiddleware)
	r.Use(obs.MetricsMiddleware)

	r.Get("/ws", s.Hub.ServeWS)

	r.Route("/api/v1", func(api chi.Router) {
		api.Post("/auth/register", s.handleRegister)
		api.Post("/auth/login", s.handleLogin)
		api.Post("/auth/refresh", s.handleRefresh)

		api.Get("/instruments", s.handleInstruments)
		api.Get("/trades", s.handleTrades)
		api.Get("/book/{symbol}", s.handleBook)
		api.Get("/candles/{symbol}", s.handleCandles)

		api.Group(func(priv chi.Router) {
			priv.Use(s.Auth.RequireAuth)
			priv.Use(s.Limiter.Middleware)

			priv.Post("/orders", s.handlePlaceOrder)
			priv.Delete("/orders/{id}", s.handleCancelOrder)
			priv.Get("/orders", s.handleListOrders)
			priv.Get("/portfolio", s.handlePortfolio)
			// Demo helper: allow users/bots to seed shares or top up virtual cash
			priv.Post("/portfolio/seed", s.handleSeedPortfolio)
		})
	})

	if baseMux != nil {
		r.Mount("/", baseMux)
	}
	return r
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key, X-Correlation-ID")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---- Auth endpoints --------------------------------------------------------

type authReq struct {
	Email        string `json:"email"`
	Password     string `json:"password"`
	RefreshToken string `json:"refresh_token"`
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req authReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "Invalid JSON", err.Error())
		return
	}
	pair, err := s.Auth.Register(r.Context(), req.Email, req.Password)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrEmailTaken):
			httpx.WriteProblem(w, r, http.StatusConflict, "Email Taken", err.Error())
		case errors.Is(err, auth.ErrInvalidCredentials):
			httpx.WriteProblem(w, r, http.StatusBadRequest, "Invalid Credentials", err.Error())
		default:
			httpx.WriteProblem(w, r, http.StatusInternalServerError, "Internal Error", err.Error())
		}
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, pair)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req authReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "Invalid JSON", err.Error())
		return
	}
	pair, err := s.Auth.Login(r.Context(), req.Email, req.Password)
	if err != nil {
		if errors.Is(err, auth.ErrInvalidCredentials) {
			httpx.WriteProblem(w, r, http.StatusUnauthorized, "Invalid Credentials", err.Error())
			return
		}
		httpx.WriteProblem(w, r, http.StatusInternalServerError, "Internal Error", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, pair)
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	var req authReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "Invalid JSON", err.Error())
		return
	}
	pair, err := s.Auth.Refresh(r.Context(), req.RefreshToken)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "Invalid Refresh Token", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, pair)
}

// ---- Market & Read endpoints -----------------------------------------------

func (s *Server) handleInstruments(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Pool.Query(r.Context(), `
		SELECT symbol, name, tick_size, lot_size, max_order_qty, ref_price, status
		FROM instruments
		ORDER BY symbol
	`)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusInternalServerError, "Database Error", err.Error())
		return
	}
	defer rows.Close()

	type instView struct {
		order.Instrument
		LastPrice int64 `json:"last_price"`
	}
	out := []instView{}
	for rows.Next() {
		var iv instView
		if err := rows.Scan(&iv.Symbol, &iv.Name, &iv.TickSize, &iv.LotSize, &iv.MaxOrderQty, &iv.RefPrice, &iv.Status); err != nil {
			httpx.WriteProblem(w, r, http.StatusInternalServerError, "Database Error", err.Error())
			return
		}
		if lp := s.Hub.LastPrice(iv.Symbol); lp > 0 {
			iv.LastPrice = lp
		} else {
			iv.LastPrice = iv.RefPrice
		}
		out = append(out, iv)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"instruments": out})
}

func (s *Server) handleBook(w http.ResponseWriter, r *http.Request) {
	sym := chi.URLParam(r, "symbol")
	depth, _ := strconv.Atoi(r.URL.Query().Get("depth"))
	httpx.WriteJSON(w, http.StatusOK, s.Hub.BookSnapshot(sym, depth))
}

func (s *Server) handleTrades(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sym := strings.ToUpper(strings.TrimSpace(q.Get("symbol")))
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	cursor, _ := strconv.ParseInt(q.Get("cursor"), 10, 64)

	rows, err := s.Pool.Query(r.Context(), `
		SELECT id, match_id, symbol, price, qty, aggressor_side, executed_at
		FROM trades
		WHERE ($1 = '' OR symbol = $1)
		  AND ($2 = 0  OR id < $2)
		ORDER BY id DESC
		LIMIT $3
	`, sym, cursor, limit)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusInternalServerError, "Database Error", err.Error())
		return
	}
	defer rows.Close()

	type tradeRow struct {
		ID            int64     `json:"id"`
		MatchID       uuid.UUID `json:"match_id"`
		Symbol        string    `json:"symbol"`
		Price         int64     `json:"price"`
		Qty           int64     `json:"qty"`
		AggressorSide string    `json:"aggressor_side"`
		ExecutedAt    time.Time `json:"executed_at"`
	}
	trades := []tradeRow{}
	var nextCursor string
	for rows.Next() {
		var tr tradeRow
		if err := rows.Scan(&tr.ID, &tr.MatchID, &tr.Symbol, &tr.Price, &tr.Qty, &tr.AggressorSide, &tr.ExecutedAt); err != nil {
			httpx.WriteProblem(w, r, http.StatusInternalServerError, "Database Error", err.Error())
			return
		}
		trades = append(trades, tr)
		nextCursor = strconv.FormatInt(tr.ID, 10)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"trades":      trades,
		"next_cursor": nextCursor,
	})
}

func (s *Server) handleCandles(w http.ResponseWriter, r *http.Request) {
	sym := strings.ToUpper(chi.URLParam(r, "symbol"))
	interval := r.URL.Query().Get("interval")
	if interval == "" {
		interval = "1m"
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 120
	}

	rows, err := s.Pool.Query(r.Context(), `
		SELECT bucket_start, open, high, low, close, volume
		FROM candles
		WHERE symbol = $1 AND interval = $2
		ORDER BY bucket_start DESC
		LIMIT $3
	`, sym, interval, limit)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusInternalServerError, "Database Error", err.Error())
		return
	}
	defer rows.Close()

	var candles []events.Candle
	for rows.Next() {
		var ts time.Time
		c := events.Candle{Symbol: sym, Interval: interval}
		if err := rows.Scan(&ts, &c.Open, &c.High, &c.Low, &c.Close, &c.Volume); err != nil {
			httpx.WriteProblem(w, r, http.StatusInternalServerError, "Database Error", err.Error())
			return
		}
		c.BucketStart = ts.Unix()
		candles = append(candles, c)
	}
	// Reverse to chronological ascending order for charting libraries.
	for i, j := 0, len(candles)-1; i < j; i, j = i+1, j-1 {
		candles[i], candles[j] = candles[j], candles[i]
	}
	if candles == nil {
		candles = []events.Candle{}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"candles": candles})
}

// ---- Portfolio & Orders ----------------------------------------------------

func (s *Server) handlePortfolio(w http.ResponseWriter, r *http.Request) {
	uid := auth.UserIDFromContext(r.Context())
	var avail, reserved int64
	err := s.Pool.QueryRow(r.Context(), `
		SELECT available, reserved FROM accounts WHERE user_id = $1 AND currency = 'USD'
	`, uid).Scan(&avail, &reserved)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusInternalServerError, "Database Error", err.Error())
		return
	}

	rows, err := s.Pool.Query(r.Context(), `
		SELECT p.symbol, p.qty, p.reserved_qty, p.avg_cost, i.ref_price
		FROM positions p
		JOIN instruments i ON i.symbol = p.symbol
		WHERE p.user_id = $1 AND (p.qty > 0 OR p.reserved_qty > 0)
		ORDER BY p.symbol
	`, uid)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusInternalServerError, "Database Error", err.Error())
		return
	}
	defer rows.Close()

	type posView struct {
		Symbol        string `json:"symbol"`
		Qty           int64  `json:"qty"`
		ReservedQty   int64  `json:"reserved_qty"`
		TotalQty      int64  `json:"total_qty"`
		AvgCost       int64  `json:"avg_cost"`
		MarkPrice     int64  `json:"mark_price"`
		MarketValue   int64  `json:"market_value"`
		UnrealisedPnL int64  `json:"unrealised_pnl"`
	}
	positions := []posView{}
	var totalMarketValue, totalUnrealisedPnL int64
	for rows.Next() {
		var p posView
		var refPx int64
		if err := rows.Scan(&p.Symbol, &p.Qty, &p.ReservedQty, &p.AvgCost, &refPx); err != nil {
			httpx.WriteProblem(w, r, http.StatusInternalServerError, "Database Error", err.Error())
			return
		}
		p.TotalQty = p.Qty + p.ReservedQty
		mark := s.Hub.LastPrice(p.Symbol)
		if mark == 0 {
			_ = s.Pool.QueryRow(r.Context(), `SELECT price FROM trades WHERE symbol = $1 ORDER BY id DESC LIMIT 1`, p.Symbol).Scan(&mark)
		}
		if mark == 0 {
			mark = refPx
		}
		p.MarkPrice = mark
		p.MarketValue = p.TotalQty * mark
		p.UnrealisedPnL = p.TotalQty * (mark - p.AvgCost)
		totalMarketValue += p.MarketValue
		totalUnrealisedPnL += p.UnrealisedPnL
		positions = append(positions, p)
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"cash_available":       avail,
		"cash_reserved":        reserved,
		"cash_total":           avail + reserved,
		"positions":            positions,
		"total_market_value":   totalMarketValue,
		"total_unrealised_pnl": totalUnrealisedPnL,
		"total_equity":         avail + reserved + totalMarketValue,
	})
}

func (s *Server) handleSeedPortfolio(w http.ResponseWriter, r *http.Request) {
	uid := auth.UserIDFromContext(r.Context())
	var req struct {
		CashCents int64  `json:"cash_cents"`
		Symbol    string `json:"symbol"`
		Shares    int64  `json:"shares"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "Invalid JSON", err.Error())
		return
	}
	if req.CashCents > 0 {
		_, err := s.Pool.Exec(r.Context(), `
			UPDATE accounts SET available = available + $1 WHERE user_id = $2 AND currency = 'USD'
		`, req.CashCents, uid)
		if err != nil {
			httpx.WriteProblem(w, r, http.StatusInternalServerError, "Database Error", err.Error())
			return
		}
	}
	if req.Symbol != "" && req.Shares > 0 {
		sym := strings.ToUpper(strings.TrimSpace(req.Symbol))
		var refPx int64
		if err := s.Pool.QueryRow(r.Context(), `SELECT ref_price FROM instruments WHERE symbol = $1`, sym).Scan(&refPx); err != nil {
			httpx.WriteProblem(w, r, http.StatusBadRequest, "Unknown Symbol", err.Error())
			return
		}
		_, err := s.Pool.Exec(r.Context(), `
			INSERT INTO positions (user_id, symbol, qty, reserved_qty, avg_cost)
			VALUES ($1, $2, $3, 0, $4)
			ON CONFLICT (user_id, symbol) DO UPDATE
			SET qty = positions.qty + EXCLUDED.qty,
			    avg_cost = CASE WHEN positions.avg_cost = 0 THEN EXCLUDED.avg_cost ELSE positions.avg_cost END
		`, uid, sym, req.Shares, refPx)
		if err != nil {
			httpx.WriteProblem(w, r, http.StatusInternalServerError, "Database Error", err.Error())
			return
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "seeded"})
}

func (s *Server) handleListOrders(w http.ResponseWriter, r *http.Request) {
	uid := auth.UserIDFromContext(r.Context())
	q := r.URL.Query()
	status := strings.ToUpper(strings.TrimSpace(q.Get("status")))
	sym := strings.ToUpper(strings.TrimSpace(q.Get("symbol")))
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var cursorTime time.Time
	if c := q.Get("cursor"); c != "" {
		if ns, err := strconv.ParseInt(c, 10, 64); err == nil {
			cursorTime = time.Unix(0, ns).UTC()
		}
	}

	rows, err := s.Pool.Query(r.Context(), `
		SELECT id, user_id, symbol, side, type, price, protection_price,
		       qty, filled_qty, reserved_amount, status, reject_reason, created_at, updated_at
		FROM orders
		WHERE user_id = $1
		  AND ($2 = '' OR status = $2)
		  AND ($3 = '' OR symbol = $3)
		  AND ($4::timestamptz IS NULL OR created_at < $4)
		ORDER BY created_at DESC, id DESC
		LIMIT $5
	`, uid, status, sym, nullableTime(cursorTime), limit)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusInternalServerError, "Database Error", err.Error())
		return
	}
	defer rows.Close()

	orders := []order.Order{}
	var nextCursor string
	for rows.Next() {
		var o order.Order
		if err := rows.Scan(&o.ID, &o.UserID, &o.Symbol, &o.Side, &o.Type, &o.Price, &o.ProtectionPrice,
			&o.Qty, &o.FilledQty, &o.ReservedAmount, &o.Status, &o.RejectReason, &o.CreatedAt, &o.UpdatedAt); err != nil {
			httpx.WriteProblem(w, r, http.StatusInternalServerError, "Database Error", err.Error())
			return
		}
		orders = append(orders, o)
		nextCursor = strconv.FormatInt(o.CreatedAt.UnixNano(), 10)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"orders":      orders,
		"next_cursor": nextCursor,
	})
}

func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

func (s *Server) handlePlaceOrder(w http.ResponseWriter, r *http.Request) {
	if s.LocalOrders != nil && s.OrderServiceURL == "" {
		r.Header.Set(order.HeaderUserID, auth.UserIDFromContext(r.Context()).String())
		(&order.Handler{Svc: s.LocalOrders}).PlaceOrder(w, r)
		return
	}
	s.proxyToOrderService(w, r, http.MethodPost, "/api/v1/orders")
}

func (s *Server) handleCancelOrder(w http.ResponseWriter, r *http.Request) {
	oid := chi.URLParam(r, "id")
	if s.LocalOrders != nil && s.OrderServiceURL == "" {
		r.Header.Set(order.HeaderUserID, auth.UserIDFromContext(r.Context()).String())
		(&order.Handler{Svc: s.LocalOrders}).CancelOrder(w, r)
		return
	}
	s.proxyToOrderService(w, r, http.MethodDelete, "/api/v1/orders/"+oid)
}

func (s *Server) proxyToOrderService(w http.ResponseWriter, r *http.Request, method, path string) {
	uid := auth.UserIDFromContext(r.Context())
	body, _ := io.ReadAll(r.Body)
	req, err := http.NewRequestWithContext(r.Context(), method, s.OrderServiceURL+path, bytes.NewReader(body))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusInternalServerError, "Proxy Error", err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(order.HeaderUserID, uid.String())
	req.Header.Set("Idempotency-Key", r.Header.Get("Idempotency-Key"))
	req.Header.Set(httpx.HeaderCorrelationID, obs.CorrelationID(r.Context()))

	resp, err := s.HTTPClient.Do(req)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadGateway, "Order Service Unreachable", err.Error())
		return
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// RunKafkaFanout consumes orders.events, trades.executed, market.ticks, and
// market.candles.1m and feeds them into the WebSocket Hub.
func (s *Server) RunKafkaFanout(ctx context.Context, cl *kgo.Client) error {
	for {
		fetches := cl.PollFetches(ctx)
		if fetches.IsClientClosed() || ctx.Err() != nil {
			return ctx.Err()
		}
		fetches.EachRecord(func(rec *kgo.Record) {
			env, err := events.Unwrap(rec.Value, nil)
			if err != nil {
				return
			}
			switch env.EventType {
			case events.TypeOrderEvent:
				var oev events.OrderEvent
				if _, err := events.Unwrap(rec.Value, &oev); err == nil {
					s.Hub.OnOrderEvent(oev)
				}
			case events.TypeTradeExecuted:
				var tr events.TradeExecuted
				if _, err := events.Unwrap(rec.Value, &tr); err == nil {
					s.Hub.OnTrade(tr)
				}
			case events.TypeMarketTick:
				var tk events.MarketTick
				if _, err := events.Unwrap(rec.Value, &tk); err == nil {
					s.Hub.OnTick(tk)
				}
			case events.TypeCandle1m:
				var cd events.Candle
				if _, err := events.Unwrap(rec.Value, &cd); err == nil {
					s.Hub.OnCandle(cd)
				}
			}
		})
	}
}
