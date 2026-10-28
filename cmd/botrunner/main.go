// Command botrunner runs the OrderFlow botrunner.
//
// Market-maker and noise-trader bots that keep the exchange alive.
package main

import (
	"os"

	"github.com/tejas004s/orderflow/internal/app"
)

func main() {
	d, err := app.Bootstrap("botrunner", ":8085", app.Needs{DB: false, Kafka: false})
	if err != nil {
		os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(1)
	}
	if err := d.Svc.Run(d.Cfg.HTTPAddr); err != nil {
		os.Exit(1)
	}
}
