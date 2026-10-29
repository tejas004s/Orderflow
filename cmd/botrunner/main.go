// Command botrunner runs the OrderFlow botrunner.
//
// Market-maker and noise-trader bots that keep the exchange alive.
package main

import (
	"os"

	"github.com/tejas004s/orderflow/internal/app"
	"github.com/tejas004s/orderflow/internal/bots"
	"github.com/tejas004s/orderflow/internal/config"
)

func main() {
	d, err := app.Bootstrap("botrunner", ":8085", app.Needs{DB: false, Kafka: false})
	if err != nil {
		os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(1)
	}

	gatewayURL := config.String("GATEWAY_URL", "http://localhost:8080")
	if config.Bool("BOTS_ENABLED", true) {
		runner := bots.NewRunner(gatewayURL, d.Log)
		d.Svc.AddWorker(runner.Run)
	}

	if err := d.Svc.Run(d.Cfg.HTTPAddr); err != nil {
		os.Exit(1)
	}
}
