package svc

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestService() *Service {
	return New("test", slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestHealthz(t *testing.T) {
	s := newTestService()
	rr := httptest.NewRecorder()
	s.Mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"ok"`) {
		t.Fatalf("got %d %s", rr.Code, rr.Body)
	}
}

func TestReadyzReflectsChecks(t *testing.T) {
	s := newTestService()
	fail := true
	s.AddCheck("dep", func(context.Context) error {
		if fail {
			return errors.New("down")
		}
		return nil
	})

	rr := httptest.NewRecorder()
	s.Mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", rr.Code)
	}

	fail = false
	rr = httptest.NewRecorder()
	s.Mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	s := newTestService()
	rr := httptest.NewRecorder()
	s.Mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "go_goroutines") {
		t.Fatalf("metrics missing: %d", rr.Code)
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	s := newTestService()
	ran := make(chan struct{})
	s.AddWorker(func(ctx context.Context) error {
		close(ran)
		<-ctx.Done()
		return ctx.Err()
	})
	closed := false
	s.OnClose(func() { closed = true })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- s.RunContext(ctx, "127.0.0.1:0") }()
	<-ran
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
	if !closed {
		t.Fatal("closer not called")
	}
}
