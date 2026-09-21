// Command checkout-service is one half of the Incident Lab: it turns a basket
// into an order by calling payment-service, and it breaks on demand.
//
// It belongs to the lab rather than to the platform. Its routes, metrics and
// environment variables come from the knowledge corpus in testdata/knowledge,
// so that a runbook the agent retrieves describes something that exists.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/wpan36/incident_diag/internal/config"
	"github.com/wpan36/incident_diag/internal/lab"
	"github.com/wpan36/incident_diag/internal/log"
	"github.com/wpan36/incident_diag/internal/shutdown"
)

const service = "checkout-service"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	cfg, cfgErr := config.Load()
	checkoutCfg, checkoutErr := config.LoadCheckout()
	if err := errors.Join(cfgErr, checkoutErr); err != nil {
		return err
	}

	w, closeLog, err := lab.LogWriter(checkoutCfg.LogDir, service)
	if err != nil {
		return err
	}
	defer closeLog()

	logger := log.New(w, cfg.LogLevel, service)
	logger.Info("starting "+service, "config", cfg.String(), "checkout", checkoutCfg.String())

	ctx, stop := shutdown.Context(context.Background())
	defer stop()

	app := lab.New(lab.Options{
		Service: service,
		Logger:  logger,
		// checkout-service adds the latency fault to its own handler. Its
		// dependency's latency is something it measures, not something it
		// injects.
		LatencyFaultInMiddleware: true,
	})
	lab.NewCheckout(app, lab.CheckoutOptions{
		PaymentURL:     checkoutCfg.PaymentURL,
		PaymentTimeout: checkoutCfg.PaymentTimeout,
	})

	return app.Run(ctx, cfg.HTTPAddr)
}
