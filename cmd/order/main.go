// Command order runs the OrderFlow order-service.
//
// Validates orders, reserves funds and publishes commands via the outbox.
package main

import (
	"os"

	"github.com/tejas004s/orderflow/internal/app"
)

func main() {
	d, err := app.Bootstrap("order-service", ":8081", app.Needs{DB: true, Kafka: true})
	if err != nil {
		os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(1)
	}
	if err := d.Svc.Run(d.Cfg.HTTPAddr); err != nil {
		os.Exit(1)
	}
}
