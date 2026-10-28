// Package svc is the common service runtime: an HTTP server exposing
// /healthz, /readyz and /metrics, background workers, and graceful shutdown
// on SIGINT/SIGTERM.
package svc

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/sync/errgroup"
)

// Check is a readiness probe. It must return quickly.
type Check func(ctx context.Context) error

// Worker is a long-running background task. It must return when ctx is done.
type Worker func(ctx context.Context) error

// Service bundles the HTTP mux, readiness checks and workers.
type Service struct {
	Name string
	Log  *slog.Logger
	Mux  *http.ServeMux

	mu      sync.Mutex
	checks  map[string]Check
	workers []Worker
	closers []func()
	handler http.Handler
}

// New creates a Service with health and metrics endpoints registered.
func New(name string, log *slog.Logger) *Service {
	s := &Service{Name: name, Log: log, Mux: http.NewServeMux(), checks: map[string]Check{}}
	s.Mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": name})
	})
	s.Mux.HandleFunc("GET /readyz", s.ready)
	s.Mux.Handle("GET /metrics", promhttp.Handler())
	return s
}

// AddCheck registers a named readiness check.
func (s *Service) AddCheck(name string, c Check) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checks[name] = c
}

// AddWorker registers a background worker started by Run.
func (s *Service) AddWorker(w Worker) { s.workers = append(s.workers, w) }

// OnClose registers a cleanup func run (in reverse order) after shutdown.
func (s *Service) OnClose(f func()) { s.closers = append(s.closers, f) }

// SetHandler overrides the root HTTP handler (e.g. a chi router that
// mounts s.Mux). Defaults to s.Mux.
func (s *Service) SetHandler(h http.Handler) { s.handler = h }

func (s *Service) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	s.mu.Lock()
	checks := make(map[string]Check, len(s.checks))
	for k, v := range s.checks {
		checks[k] = v
	}
	s.mu.Unlock()

	res := map[string]string{}
	ok := true
	for name, c := range checks {
		if err := c(ctx); err != nil {
			res[name] = err.Error()
			ok = false
		} else {
			res[name] = "ok"
		}
	}
	code := http.StatusOK
	if !ok {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]any{"ready": ok, "checks": res})
}

// Run starts the HTTP server and all workers and blocks until a signal is
// received or any worker fails. It then shuts everything down gracefully.
func (s *Service) Run(addr string) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return s.RunContext(ctx, addr)
}

// RunContext is Run with a caller-supplied context (useful in tests).
func (s *Service) RunContext(ctx context.Context, addr string) error {
	h := s.handler
	if h == nil {
		h = s.Mux
	}
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 5 * time.Second}

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		s.Log.Info("http listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})
	g.Go(func() error {
		<-gctx.Done()
		shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shCtx)
	})
	for _, w := range s.workers {
		g.Go(func() error {
			err := w(gctx)
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		})
	}
	err := g.Wait()
	for i := len(s.closers) - 1; i >= 0; i-- {
		s.closers[i]()
	}
	if err != nil {
		s.Log.Error("service stopped with error", "err", err)
		return err
	}
	s.Log.Info("service stopped")
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
