// Command ledger runs the OrderFlow ledger-service.
//
// Settles trades idempotently into the double-entry ledger.
package main

import (
	"context"
	"os"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/tejas004s/orderflow/internal/app"
	"github.com/tejas004s/orderflow/internal/kafka"
	"github.com/tejas004s/orderflow/internal/ledger"
	"github.com/tejas004s/orderflow/internal/outbox"
)

func main() {
	d, err := app.Bootstrap("ledger-service", ":8083", app.Needs{DB: true, Kafka: true})
	if err != nil {
		os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(1)
	}

	prod, err := kgo.NewClient(kafka.ProducerOpts(d.Cfg.KafkaBrokers, "ledger-service-producer")...)
	if err != nil {
		d.Log.Error("kafka producer init failed", "err", err)
		os.Exit(1)
	}
	d.Svc.OnClose(prod.Close)

	cons, err := kgo.NewClient(
		kafka.ConsumerOpts(
			d.Cfg.KafkaBrokers,
			"ledger-service-consumer",
			"ledger-service",
			kafka.TopicTrades,
			kafka.TopicOrderEvents,
		)...,
	)
	if err != nil {
		d.Log.Error("kafka consumer init failed", "err", err)
		os.Exit(1)
	}
	d.Svc.OnClose(cons.Close)

	svc := ledger.NewService(d.DB, outbox.FranzPublisher{Client: prod}, d.Log)
	d.Svc.AddWorker(func(ctx context.Context) error {
		return svc.RunKafkaLoop(ctx, cons)
	})

	if err := d.Svc.Run(d.Cfg.HTTPAddr); err != nil {
		os.Exit(1)
	}
}
