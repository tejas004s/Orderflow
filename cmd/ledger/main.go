// Command ledger runs the OrderFlow ledger-service.
//
// Settles trades idempotently into the double-entry ledger.
package main

import (
	"os"

	"github.com/tejas004s/orderflow/internal/app"
)

func main() {
	d, err := app.Bootstrap("ledger-service", ":8083", app.Needs{DB: true, Kafka: true})
	if err != nil {
		os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(1)
	}
	if err := d.Svc.Run(d.Cfg.HTTPAddr); err != nil {
		os.Exit(1)
	}
}
