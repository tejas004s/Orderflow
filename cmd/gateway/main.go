// Command gateway runs the OrderFlow api-gateway.
//
// REST + WebSocket entry point: auth, rate limiting, live fan-out.
package main

import (
	"os"

	"github.com/tejas004s/orderflow/internal/app"
)

func main() {
	d, err := app.Bootstrap("api-gateway", ":8080", app.Needs{DB: true, Kafka: true})
	if err != nil {
		os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(1)
	}
	if err := d.Svc.Run(d.Cfg.HTTPAddr); err != nil {
		os.Exit(1)
	}
}
