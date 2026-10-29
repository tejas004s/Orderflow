package obs

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Prometheus metrics specified in SDD section 12.
var (
	OrdersTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "orderflow_orders_total",
		Help: "Total orders processed by side, type, and status.",
	}, []string{"side", "type", "status"})

	TradesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "orderflow_trades_executed_total",
		Help: "Total trades matched by symbol.",
	}, []string{"symbol"})

	MatchLatencySeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "orderflow_match_latency_seconds",
		Help:    "Matching engine command processing latency in seconds.",
		Buckets: []float64{1e-6, 2.5e-6, 5e-6, 1e-5, 2.5e-5, 5e-5, 1e-4, 5e-4, 1e-3, 5e-3, 1e-2},
	})

	OutboxBacklog = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "orderflow_outbox_published_batch_size",
		Help: "Number of outbox messages published in the latest relay batch.",
	})

	WSConnections = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "orderflow_ws_connections",
		Help: "Current number of active WebSocket client connections.",
	})

	HTTPRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "orderflow_http_request_duration_seconds",
		Help:    "HTTP request latency in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "status"})
)

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// MetricsMiddleware records HTTP request count and duration.
func MetricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		HTTPRequestDuration.WithLabelValues(r.Method, strconv.Itoa(rec.status)).Observe(time.Since(start).Seconds())
	})
}
