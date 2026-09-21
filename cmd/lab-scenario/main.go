// Command lab-scenario drives the Incident Lab into one of the failures the
// knowledge corpus describes and reports whether the numbers moved.
//
// It is a script rather than a test: it is how a failure is reproduced for a
// demonstration, for a manual look at Grafana, or as the starting point of an
// agent run. It clears every fault on its way out, including after a Ctrl-C.
//
//	lab-scenario list
//	lab-scenario payment-latency
//	lab-scenario checkout-cpu -hold 5m
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/wpan36/incident_diag/internal/lab/scenario"
	"github.com/wpan36/incident_diag/internal/shutdown"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	defaults := scenario.DefaultParams()

	cfg := scenario.Config{Out: os.Stdout}
	flag.StringVar(&cfg.CheckoutURL, "checkout", "http://127.0.0.1:8081", "checkout-service's base URL")
	flag.StringVar(&cfg.PaymentURL, "payment", "http://127.0.0.1:8082", "payment-service's base URL")
	flag.Float64Var(&cfg.RPS, "rps", 7, "target request rate; demand is rps × hold time, so this decides whether the pool saturates")
	flag.DurationVar(&cfg.Baseline, "baseline", 30*time.Second, "how long to measure before the fault")
	flag.DurationVar(&cfg.Hold, "hold", 2*time.Minute, "how long to hold the fault; the alerts the runbooks quote need 5m")
	flag.DurationVar(&cfg.Recovery, "recovery", 30*time.Second, "how long to measure after the fault is cleared")
	flag.DurationVar(&cfg.RequestTimeout, "timeout", 10*time.Second, "the generator's own patience; must exceed CHECKOUT_PAYMENT_TIMEOUT")
	flag.IntVar(&cfg.MaxInFlight, "max-in-flight", 500, "cap on concurrent requests; reaching it is reported")

	flag.IntVar(&cfg.Params.DelayMS, "delay", defaults.DelayMS, "latency fault: added delay in milliseconds")
	flag.IntVar(&cfg.Params.JitterMS, "jitter", defaults.JitterMS, "latency fault: jitter around the delay, in milliseconds")
	flag.IntVar(&cfg.Params.Status, "status", defaults.Status, "error fault: the status to answer with")
	flag.Float64Var(&cfg.Params.Ratio, "ratio", defaults.Ratio, "error fault: the share of requests to fail")
	flag.IntVar(&cfg.Params.Workers, "workers", defaults.Workers, "cpu fault: how many goroutines to spin")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: lab-scenario [flags] <%s|list>\n\n", strings.Join(scenario.Names(), "|"))
		flag.PrintDefaults()
	}
	// The scenario name may come before the flags, which is how anyone would
	// type it. Go's flag package stops at the first non-flag argument, so it is
	// pulled out before parsing.
	args := os.Args[1:]
	var name string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	if err := flag.CommandLine.Parse(args); err != nil {
		return err
	}
	if name == "" {
		if flag.NArg() != 1 {
			flag.Usage()
			return fmt.Errorf("exactly one scenario name is required")
		}
		name = flag.Arg(0)
	} else if flag.NArg() != 0 {
		flag.Usage()
		return fmt.Errorf("unexpected arguments after the scenario name: %s", strings.Join(flag.Args(), " "))
	}
	if name == "list" {
		fmt.Print(scenario.List())
		return nil
	}

	s, err := scenario.Find(name)
	if err != nil {
		return err
	}
	if cfg.RPS <= 0 {
		return fmt.Errorf("-rps must be greater than zero")
	}
	if cfg.MaxInFlight < 1 {
		return fmt.Errorf("-max-in-flight must be at least 1")
	}

	// A Ctrl-C cancels the run, and Run's deferred cleanup still clears the
	// faults: leaving the lab broken would make the next scenario measure the
	// wrong incident.
	ctx, stop := shutdown.Context(context.Background())
	defer stop()

	return scenario.Run(ctx, cfg, s)
}
