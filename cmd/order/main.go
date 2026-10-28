// Command order runs the OrderFlow order-service.
//
// Validates orders, reserves funds and publishes commands via the outbox.
package main

import (
	"os"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/tejas004s/orderflow/internal/app"
	"github.com/tejas004s/orderflow/internal/kafka"
	"github.com/tejas004s/orderflow/internal/order"
	"github.com/tejas004s/orderflow/internal/outbox"
)

func main() {
	d, err := app.Bootstrap("order-service", ":8081", app.Needs{DB: true, Kafka: true})
	if err != nil {
		os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(1)
	}

	prod, err := kgo.NewClient(kafka.ProducerOpts(d.Cfg.KafkaBrokers, "order-service-producer")...)
	if err != nil {
		d.Log.Error("kafka producer init failed", "err", err)
		os.Exit(1)
	}
	d.Svc.OnClose(prod.Close)

	relay := outbox.NewRelay(d.DB, outbox.FranzPublisher{Client: prod}, d.Log)
	d.Svc.AddWorker(relay.Run)

	svc := order.NewService(d.DB, relay)
	d.Svc.SetHandler(order.NewRouter(svc, d.Svc.Mux))

	if err := d.Svc.Run(d.Cfg.HTTPAddr); err != nil {
		os.Exit(1)
	}
}
