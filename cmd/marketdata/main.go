// Command marketdata runs the OrderFlow marketdata-service.
//
// Simulates price ticks and aggregates OHLCV candles.
package main

import (
	"context"
	"os"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/tejas004s/orderflow/internal/app"
	"github.com/tejas004s/orderflow/internal/kafka"
	"github.com/tejas004s/orderflow/internal/marketdata"
	"github.com/tejas004s/orderflow/internal/outbox"
)

func main() {
	d, err := app.Bootstrap("marketdata-service", ":8084", app.Needs{DB: true, Kafka: true})
	if err != nil {
		os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(1)
	}

	prod, err := kgo.NewClient(kafka.ProducerOpts(d.Cfg.KafkaBrokers, "marketdata-producer")...)
	if err != nil {
		d.Log.Error("kafka producer init failed", "err", err)
		os.Exit(1)
	}
	d.Svc.OnClose(prod.Close)

	cons, err := kgo.NewClient(
		kafka.ConsumerOpts(
			d.Cfg.KafkaBrokers,
			"marketdata-consumer",
			"marketdata-service",
			kafka.TopicTrades,
		)...,
	)
	if err != nil {
		d.Log.Error("kafka consumer init failed", "err", err)
		os.Exit(1)
	}
	d.Svc.OnClose(cons.Close)

	svc := marketdata.NewService(d.DB, outbox.FranzPublisher{Client: prod}, d.Log)
	if err := svc.Init(context.Background()); err != nil {
		d.Log.Error("marketdata init failed", "err", err)
		os.Exit(1)
	}

	d.Svc.AddWorker(svc.RunSimulator)
	d.Svc.AddWorker(func(ctx context.Context) error {
		return svc.RunTradeConsumer(ctx, cons)
	})

	if err := d.Svc.Run(d.Cfg.HTTPAddr); err != nil {
		os.Exit(1)
	}
}
