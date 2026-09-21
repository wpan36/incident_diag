// Command payment-service is the other half of the Incident Lab: it authorizes
// charges through a fixed-size pool of connections to a simulated card
// processor.
//
// The pool is the model. Every failure the lab can produce comes from holding
// those connections longer, which is why the latency fault here is added inside
// the processor call rather than around the handler.
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

const service = "payment-service"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	cfg, cfgErr := config.Load()
	paymentCfg, paymentErr := config.LoadPayment()
	if err := errors.Join(cfgErr, paymentErr); err != nil {
		return err
	}

	w, closeLog, err := lab.LogWriter(paymentCfg.LogDir, service)
	if err != nil {
		return err
	}
	defer closeLog()

	logger := log.New(w, cfg.LogLevel, service)
	logger.Info("starting "+service, "config", cfg.String(), "payment", paymentCfg.String())

	ctx, stop := shutdown.Context(context.Background())
	defer stop()

	app := lab.New(lab.Options{Service: service, Logger: logger})
	lab.NewPayment(app, lab.PaymentOptions{
		PoolSize:         paymentCfg.PoolSize,
		ProcessorLatency: paymentCfg.ProcessorLatency,
	})

	return app.Run(ctx, cfg.HTTPAddr)
}
