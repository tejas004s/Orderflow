// Command gateway runs the OrderFlow api-gateway.
//
// REST + WebSocket entry point: auth, rate limiting, live fan-out.
package main

import (
	"context"
	"os"

	"github.com/google/uuid"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/tejas004s/orderflow/internal/app"
	"github.com/tejas004s/orderflow/internal/auth"
	"github.com/tejas004s/orderflow/internal/config"
	"github.com/tejas004s/orderflow/internal/gateway"
	"github.com/tejas004s/orderflow/internal/gateway/ws"
	"github.com/tejas004s/orderflow/internal/kafka"
)

func main() {
	d, err := app.Bootstrap("api-gateway", ":8080", app.Needs{DB: true, Kafka: true})
	if err != nil {
		os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(1)
	}

	jwtSecret := config.String("JWT_SECRET", "dev-only-change-me-dev-only-change-me")
	orderURL := config.String("ORDER_SERVICE_URL", "http://localhost:8081")

	authSvc := auth.NewService(d.DB, jwtSecret)
	hub := ws.NewHub(authSvc, d.Log)
	srv := gateway.NewServer(d.DB, authSvc, hub, orderURL, d.Log)

	// Each gateway replica needs the full fan-out stream to maintain its
	// in-memory book projection and WebSocket subscriptions.
	groupID := "api-gateway-" + uuid.NewString()[:8]
	cons, err := kgo.NewClient(
		kafka.ConsumerOpts(
			d.Cfg.KafkaBrokers,
			"api-gateway-fanout",
			groupID,
			kafka.TopicOrderEvents,
			kafka.TopicTrades,
			kafka.TopicTicks,
			kafka.TopicCandles1m,
		)...,
	)
	if err != nil {
		d.Log.Error("kafka fan-out consumer init failed", "err", err)
		os.Exit(1)
	}
	d.Svc.OnClose(cons.Close)

	d.Svc.AddWorker(func(ctx context.Context) error {
		return srv.RunKafkaFanout(ctx, cons)
	})
	d.Svc.SetHandler(srv.Router(d.Svc.Mux))

	if err := d.Svc.Run(d.Cfg.HTTPAddr); err != nil {
		os.Exit(1)
	}
}
