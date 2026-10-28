package order

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/tejas004s/orderflow/internal/httpx"
)

// HeaderUserID is passed from api-gateway to order-service on internal calls.
const HeaderUserID = "X-User-ID"

// Handler exposes the order-service HTTP endpoints.
type Handler struct {
	Svc *Service
}

// NewRouter mounts the internal order-service endpoints alongside baseMux
// (/healthz, /readyz, /metrics).
func NewRouter(s *Service, baseMux *http.ServeMux) http.Handler {
	r := chi.NewRouter()
	r.Use(httpx.CorrelationMiddleware)
	h := &Handler{Svc: s}

	r.Post("/api/v1/orders", h.PlaceOrder)
	r.Get("/api/v1/orders", h.ListOrders)
	r.Get("/api/v1/orders/{id}", h.GetOrder)
	r.Delete("/api/v1/orders/{id}", h.CancelOrder)

	if baseMux != nil {
		r.Mount("/", baseMux)
	}
	return r
}

func userIDFromRequest(r *http.Request, bodyUserID uuid.UUID) (uuid.UUID, error) {
	if hdr := r.Header.Get(HeaderUserID); hdr != "" {
		return uuid.Parse(hdr)
	}
	if bodyUserID != uuid.Nil {
		return bodyUserID, nil
	}
	return uuid.Nil, errors.New("missing X-User-ID header")
}

// PlaceOrder handles POST /api/v1/orders.
func (h *Handler) PlaceOrder(w http.ResponseWriter, r *http.Request) {
	var req PlaceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "Invalid JSON", err.Error())
		return
	}
	uid, err := userIDFromRequest(r, req.UserID)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "Unauthorized", err.Error())
		return
	}
	req.UserID = uid
	if req.IdempotencyKey == "" {
		req.IdempotencyKey = r.Header.Get("Idempotency-Key")
	}

	ord, replay, err := h.Svc.PlaceOrder(r.Context(), req)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	status := http.StatusAccepted
	if replay {
		status = http.StatusOK
	}
	httpx.WriteJSON(w, status, ord)
}

// CancelOrder handles DELETE /api/v1/orders/{id}.
func (h *Handler) CancelOrder(w http.ResponseWriter, r *http.Request) {
	uid, err := userIDFromRequest(r, uuid.Nil)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "Unauthorized", err.Error())
		return
	}
	oid, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "Invalid Order ID", err.Error())
		return
	}
	ord, err := h.Svc.CancelOrder(r.Context(), uid, oid)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, ord)
}

// GetOrder handles GET /api/v1/orders/{id}.
func (h *Handler) GetOrder(w http.ResponseWriter, r *http.Request) {
	uid, err := userIDFromRequest(r, uuid.Nil)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "Unauthorized", err.Error())
		return
	}
	oid, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "Invalid Order ID", err.Error())
		return
	}
	ord, err := h.Svc.GetOrder(r.Context(), uid, oid)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, ord)
}

// ListOrders handles GET /api/v1/orders.
func (h *Handler) ListOrders(w http.ResponseWriter, r *http.Request) {
	uid, err := userIDFromRequest(r, uuid.Nil)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "Unauthorized", err.Error())
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	orders, err := h.Svc.ListOrders(r.Context(), uid, q.Get("status"), q.Get("symbol"), limit)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"orders": orders})
}

func writeDomainError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrValidation):
		httpx.WriteProblem(w, r, http.StatusBadRequest, "Validation Failed", err.Error())
	case errors.Is(err, ErrInsufficientFunds), errors.Is(err, ErrInsufficientShares):
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "Insufficient Balance", err.Error())
	case errors.Is(err, ErrIdempotencyConflict):
		httpx.WriteProblem(w, r, http.StatusConflict, "Idempotency Conflict", err.Error())
	case errors.Is(err, ErrOrderNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, "Order Not Found", err.Error())
	case errors.Is(err, ErrOrderNotCancelable):
		httpx.WriteProblem(w, r, http.StatusConflict, "Order Not Cancelable", err.Error())
	default:
		httpx.WriteProblem(w, r, http.StatusInternalServerError, "Internal Error", err.Error())
	}
}

