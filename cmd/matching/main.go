// Command matching runs the OrderFlow matching-engine.
//
// Maintains per-symbol order books and matches by price-time priority.
package main

import (
	"context"
	"os"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/tejas004s/orderflow/internal/app"
	"github.com/tejas004s/orderflow/internal/kafka"
	"github.com/tejas004s/orderflow/internal/matching"
	"github.com/tejas004s/orderflow/internal/outbox"
)

func main() {
	d, err := app.Bootstrap("matching-engine", ":8082", app.Needs{DB: true, Kafka: true})
	if err != nil {
		os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(1)
	}

	prod, err := kgo.NewClient(kafka.ProducerOpts(d.Cfg.KafkaBrokers, "matching-engine-producer")...)
	if err != nil {
		d.Log.Error("kafka producer init failed", "err", err)
		os.Exit(1)
	}
	d.Svc.OnClose(prod.Close)

	proc := matching.NewProcessor(
		outbox.FranzPublisher{Client: prod},
		matching.PGSnapshotStore{Pool: d.DB},
		d.Log,
	)

	opts := kafka.ConsumerOpts(d.Cfg.KafkaBrokers, "matching-engine-consumer", "matching-engine", kafka.TopicOrderCommands)
	opts = append(opts, kgo.OnPartitionsAssigned(func(ctx context.Context, cl *kgo.Client, assigned map[string][]int32) {
		for _, parts := range assigned {
			for _, part := range parts {
				if nextOff, err := proc.InitPartition(ctx, part); err != nil {
					d.Log.Error("init partition snapshot failed", "partition", part, "err", err)
				} else if nextOff > 0 {
					d.Log.Info("restored partition from snapshot", "partition", part, "next_offset", nextOff)
				}
			}
		}
	}))
	cons, err := kgo.NewClient(opts...)
	if err != nil {
		d.Log.Error("kafka consumer init failed", "err", err)
		os.Exit(1)
	}
	d.Svc.OnClose(cons.Close)

	d.Svc.AddWorker(func(ctx context.Context) error {
		return proc.RunKafkaLoop(ctx, cons)
	})
	d.Svc.SetHandler(matching.NewRouter(proc, d.Svc.Mux))

	if err := d.Svc.Run(d.Cfg.HTTPAddr); err != nil {
		os.Exit(1)
	}
}
