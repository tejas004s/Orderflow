// Command matching runs the OrderFlow matching-engine.
//
// Maintains per-symbol order books and matches by price-time priority.
package main

import (
	"os"

	"github.com/tejas004s/orderflow/internal/app"
)

func main() {
	d, err := app.Bootstrap("matching-engine", ":8082", app.Needs{DB: true, Kafka: true})
	if err != nil {
		os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(1)
	}
	if err := d.Svc.Run(d.Cfg.HTTPAddr); err != nil {
		os.Exit(1)
	}
}
