// Package httpx provides RFC 7807 problem+json responses and correlation-id
// HTTP middleware used by gateway and order-service.
package httpx

import (
	"encoding/json"
	"net/http"

	"github.com/google/uuid"

	"github.com/tejas004s/orderflow/internal/obs"
)

// HeaderCorrelationID is the HTTP header carrying the request correlation ID.
const HeaderCorrelationID = "X-Correlation-ID"

// Problem is an RFC 7807 error response body (SDD section 8.1).
type Problem struct {
	Type          string `json:"type"`
	Title         string `json:"title"`
	Status        int    `json:"status"`
	Detail        string `json:"detail,omitempty"`
	Instance      string `json:"instance,omitempty"`
	CorrelationID string `json:"correlation_id,omitempty"`
}

// WriteProblem writes an RFC 7807 Problem JSON response.
func WriteProblem(w http.ResponseWriter, r *http.Request, status int, title, detail string) {
	p := Problem{
		Type:          "about:blank",
		Title:         title,
		Status:        status,
		Detail:        detail,
		Instance:      r.URL.Path,
		CorrelationID: obs.CorrelationID(r.Context()),
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(p)
}

// WriteJSON writes v as JSON with the given HTTP status code.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// CorrelationMiddleware reads or generates X-Correlation-ID and attaches it to
// r.Context() and the response header.
func CorrelationMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cid := r.Header.Get(HeaderCorrelationID)
		if cid == "" {
			cid = uuid.NewString()
		}
		w.Header().Set(HeaderCorrelationID, cid)
		next.ServeHTTP(w, r.WithContext(obs.WithCorrelationID(r.Context(), cid)))
	})
}

