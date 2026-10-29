// Package gateway implements the api-gateway HTTP server, per-user rate
// limiting, REST endpoints (/api/v1), and Kafka fan-out into the WebSocket hub
// (SDD sections 3.1, 8.1, 8.2, 13).
package gateway

import (
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/tejas004s/orderflow/internal/auth"
	"github.com/tejas004s/orderflow/internal/httpx"
)

type bucket struct {
	tokens float64
	last   time.Time
}

// RateLimiter is a per-user token-bucket rate limiter.
type RateLimiter struct {
	Rate  float64 // tokens per second
	Burst float64 // max bucket capacity

	mu      sync.Mutex
	buckets map[uuid.UUID]*bucket
}

// NewRateLimiter creates a per-user rate limiter.
func NewRateLimiter(rate, burst float64) *RateLimiter {
	return &RateLimiter{
		Rate:    rate,
		Burst:   burst,
		buckets: map[uuid.UUID]*bucket{},
	}
}

// Allow reports whether uid may perform one request at now.
func (l *RateLimiter) Allow(uid uuid.UUID) bool {
	if l == nil || l.Rate <= 0 {
		return true
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[uid]
	if !ok {
		l.buckets[uid] = &bucket{tokens: l.Burst - 1, last: now}
		return true
	}
	elapsed := now.Sub(b.last).Seconds()
	b.last = now
	b.tokens = min(l.Burst, b.tokens+elapsed*l.Rate)
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Middleware enforces the rate limit for authenticated users.
func (l *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid := auth.UserIDFromContext(r.Context())
		if uid != uuid.Nil && !l.Allow(uid) {
			httpx.WriteProblem(w, r, http.StatusTooManyRequests, "Too Many Requests", "Rate limit exceeded")
			return
		}
		next.ServeHTTP(w, r)
	})
}
