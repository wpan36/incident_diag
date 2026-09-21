package scenario

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"text/tabwriter"
	"time"

	"github.com/wpan36/incident_diag/internal/lab"
)

// gaugeSampleInterval is how often payment_pool_in_use is read during a phase.
// The in-use ratio is the first number the payment latency runbook asks for,
// and reading it once at the end would miss the shape entirely.
const gaugeSampleInterval = 2 * time.Second

// healthTimeout bounds waiting for the lab to come up. The services have no
// Compose healthcheck — the image has no shell — so this is where a stack that
// is still starting is waited for.
const healthTimeout = 60 * time.Second

// Config is one run.
type Config struct {
	CheckoutURL string
	PaymentURL  string

	// RPS is the target request rate. Demand is rps × hold time, so this and
	// the injected latency together decide whether the pool saturates.
	RPS float64

	Baseline time.Duration
	Hold     time.Duration
	Recovery time.Duration

	// RequestTimeout is the generator's own patience. It must exceed
	// checkout-service's client timeout, or the generator gives up first and
	// the 504s never appear in the report.
	RequestTimeout time.Duration

	// MaxInFlight bounds the generator's goroutines. Reaching it is reported.
	MaxInFlight int

	Params Params
	Out    io.Writer
}

// Run drives one scenario and writes its report.
func Run(ctx context.Context, cfg Config, s Scenario) error {
	client := &http.Client{Transport: pooledTransport(cfg.MaxInFlight)}

	if err := waitForHealth(ctx, client, cfg); err != nil {
		return err
	}
	// Start from a known state: a previous run that was killed may have left a
	// fault enabled, and a scenario measuring someone else's incident is worse
	// than no measurement.
	if err := clearFaults(ctx, client, cfg); err != nil {
		return err
	}
	// And leave one. context.WithoutCancel, because this has to run after a
	// Ctrl-C too: a scenario that leaves the lab broken makes the next one
	// meaningless.
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := clearFaults(cleanupCtx, client, cfg); err != nil {
			fmt.Fprintf(cfg.Out, "\nWARNING: the faults could not be cleared: %v\n", err)
		}
	}()

	gen := &generator{
		client:         client,
		checkoutURL:    cfg.CheckoutURL,
		rps:            cfg.RPS,
		maxInFlight:    cfg.MaxInFlight,
		requestTimeout: cfg.RequestTimeout,
	}

	fmt.Fprintf(cfg.Out, "scenario %s — %s\nexpect:  %s\nrunbook: %s\n\n",
		s.Name, s.Summary, s.Expect, s.Runbook)

	// The middle phase is named for what happens in it. The healthy control
	// injects nothing, and calling its middle phase "fault" would put a word in
	// the report that contradicts the run.
	middle := "fault"
	if s.Fault == nil {
		middle = "hold"
	}
	phases := []phase{
		{"baseline", cfg.Baseline},
		{middle, cfg.Hold},
		{"recovery", cfg.Recovery},
	}
	results := make([]*phaseStats, 0, len(phases))

	for _, p := range phases {
		switch {
		case p.name == middle && s.Fault != nil:
			call := s.Fault(cfg.Params)
			if err := postFault(ctx, client, cfg, call.target, call.body); err != nil {
				return err
			}
			fmt.Fprintf(cfg.Out, "[%s] fault enabled on %s: %s\n", p.name, call.target, mustJSON(call.body))
		case p.name == "recovery" && s.Fault != nil:
			if err := clearFaults(ctx, client, cfg); err != nil {
				return err
			}
			fmt.Fprintf(cfg.Out, "[%s] faults cleared\n", p.name)
		default:
			fmt.Fprintf(cfg.Out, "[%s] no fault\n", p.name)
		}

		stats, err := runPhase(ctx, client, cfg, gen, p)
		if err != nil {
			return err
		}
		results = append(results, stats)

		if ctx.Err() != nil {
			fmt.Fprintf(cfg.Out, "\ninterrupted during the %s phase\n", p.name)
			break
		}
	}

	report(cfg.Out, results)
	return nil
}

// runPhase runs the generator for one phase while sampling the pool gauge, and
// takes a metrics snapshot on each side of it.
func runPhase(ctx context.Context, client *http.Client, cfg Config, gen *generator, p phase) (*phaseStats, error) {
	// The snapshots are taken with a context that survives cancellation, so an
	// interrupted run still reports what it measured.
	snapCtx := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	}

	sctx, cancel := snapCtx()
	before, err := scrapeBoth(sctx, client, cfg)
	cancel()
	if err != nil {
		return nil, err
	}

	sampleCtx, stopSampling := context.WithCancel(ctx)
	samples := make(chan []float64, 1)
	go func() { samples <- samplePool(sampleCtx, client, cfg) }()

	stats := gen.run(ctx, p.name, p.duration)

	stopSampling()
	stats.poolInUse = <-samples

	sctx, cancel = snapCtx()
	after, err := scrapeBoth(sctx, client, cfg)
	cancel()
	if err != nil {
		return nil, err
	}
	stats.before, stats.after = before, after
	return stats, nil
}

func scrapeBoth(ctx context.Context, client *http.Client, cfg Config) (snapshot, error) {
	// Only payment-service's own metrics and checkout-service's client metrics
	// are reported, and merging the two snapshots keeps the accessors simple:
	// no metric name is exported by both.
	checkout, err := scrape(ctx, client, cfg.CheckoutURL)
	if err != nil {
		return snapshot{}, err
	}
	payment, err := scrape(ctx, client, cfg.PaymentURL)
	if err != nil {
		return snapshot{}, err
	}
	return merge(checkout, payment), nil
}

// merge combines two services' snapshots. http_request_duration_seconds is
// exported by both, so its per-route keys are prefixed with the service.
func merge(checkout, payment snapshot) snapshot {
	out := snapshot{
		at:         checkout.at,
		gauges:     map[string]float64{},
		counters:   map[string]float64{},
		histograms: map[string]histogram{},
		requests:   map[string]uint64{},
	}
	for name, v := range checkout.gauges {
		out.gauges[name] = v
	}
	for name, v := range payment.gauges {
		out.gauges[name] = v
	}
	for name, v := range checkout.counters {
		out.counters[name] = v
	}
	for name, v := range payment.counters {
		out.counters[name] = v
	}
	for name, v := range checkout.histograms {
		out.histograms[name] = v
	}
	for name, v := range payment.histograms {
		// http_request_duration_seconds is the one name both export. Summing
		// the two would produce a mean nobody measured, so payment's copy is
		// kept under its own name.
		if _, clash := out.histograms[name]; clash {
			out.histograms["payment:"+name] = v
			continue
		}
		out.histograms[name] = v
	}
	for key, v := range checkout.requests {
		out.requests["checkout-service "+key] = v
	}
	for key, v := range payment.requests {
		out.requests["payment-service "+key] = v
	}
	return out
}

// samplePool reads payment_pool_in_use until ctx is cancelled.
func samplePool(ctx context.Context, client *http.Client, cfg Config) []float64 {
	var values []float64
	ticker := time.NewTicker(gaugeSampleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return values
		case <-ticker.C:
		}
		readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		s, err := scrape(readCtx, client, cfg.PaymentURL)
		cancel()
		if err == nil {
			values = append(values, s.gauge(lab.MetricPoolInUse))
		}
	}
}

func waitForHealth(ctx context.Context, client *http.Client, cfg Config) error {
	deadline := time.Now().Add(healthTimeout)
	for _, url := range []string{cfg.CheckoutURL, cfg.PaymentURL} {
		for {
			if err := probe(ctx, client, url+"/health"); err == nil {
				break
			} else if time.Now().After(deadline) {
				return fmt.Errorf("%s is not healthy: %w", url, err)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		}
	}
	return nil
}

func probe(ctx context.Context, client *http.Client, url string) error {
	reqCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", url, resp.Status)
	}
	return nil
}

func postFault(ctx context.Context, client *http.Client, cfg Config, target string, body map[string]any) error {
	url, err := faultURL(cfg, target)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("enabling the fault on %s: %w", target, err)
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("enabling the fault on %s: %s: %s", target, resp.Status, bytes.TrimSpace(answer))
	}
	return nil
}

func clearFaults(ctx context.Context, client *http.Client, cfg Config) error {
	for _, target := range []string{targetCheckout, targetPayment} {
		url, err := faultURL(cfg, target)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("clearing the faults on %s: %w", target, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("clearing the faults on %s: %s", target, resp.Status)
		}
	}
	return nil
}

func faultURL(cfg Config, target string) (string, error) {
	switch target {
	case targetCheckout:
		return cfg.CheckoutURL + "/fault", nil
	case targetPayment:
		return cfg.PaymentURL + "/fault", nil
	}
	return "", fmt.Errorf("unknown fault target %q", target)
}

func pooledTransport(maxInFlight int) *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	// net/http keeps two idle connections per host by default, which under load
	// means a new connection for nearly every request and latency the
	// generator invented rather than measured.
	t.MaxIdleConns = maxInFlight
	t.MaxIdleConnsPerHost = maxInFlight
	t.MaxConnsPerHost = 0
	return t
}

func mustJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(raw)
}

// report renders every phase.
func report(out io.Writer, phases []*phaseStats) {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)

	fmt.Fprintln(w, "\nphase\troute\tstatuses\tp50\tp95\tp99")
	for _, p := range phases {
		for _, route := range []string{routeCreateOrder, routeGetOrder} {
			r, ok := p.routes[route]
			if !ok {
				// Printed rather than omitted. GET /orders/:id disappears from
				// the fault phase of a scenario that stops orders being created
				// at all, and a missing row reads as a formatting accident
				// rather than as the result it is.
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", p.name, route, "none reached this route", "-", "-", "-")
				continue
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", p.name, route, statusSummary(r),
				round(percentile(r.latencies, 50)), round(percentile(r.latencies, 95)),
				round(percentile(r.latencies, 99)))
		}
	}

	fmt.Fprintln(w, "\nphase\tpool in use (mean/max of size)\tpool wait (mean)\tprocessor (mean)\tclient timeouts\tpayment 5xx")
	for _, p := range phases {
		mean, max := meanMax(p.poolInUse)
		size := p.after.gauge(lab.MetricPoolSize)
		_, waitMean := histogramDelta(p.before, p.after, lab.MetricPoolWait)
		_, procMean := histogramDelta(p.before, p.after, lab.MetricProcessorLatency)
		timeouts := counterDelta(p.before, p.after, lab.MetricClientTimeouts)
		payment5xx := requestsByStatus(p.before, p.after, "payment-service", "5")

		fmt.Fprintf(w, "%s\t%.1f/%.1f of %.0f\t%s\t%s\t%.0f\t%d\n",
			p.name, mean, max, size, round(waitMean), round(procMean), timeouts, payment5xx)
	}

	for _, p := range phases {
		if p.skipped > 0 {
			fmt.Fprintf(w, "\n%s: %d dispatches were refused because the in-flight bound was reached; "+
				"the demand above understates the real load\n", p.name, p.skipped)
		}
	}
	fmt.Fprintln(w)
	w.Flush()
}

func meanMax(values []float64) (mean, max float64) {
	if len(values) == 0 {
		return 0, 0
	}
	var sum float64
	for _, v := range values {
		sum += v
		if v > max {
			max = v
		}
	}
	return sum / float64(len(values)), max
}

// round trims a duration to something readable in a table.
func round(d time.Duration) time.Duration {
	switch {
	case d >= time.Second:
		return d.Round(10 * time.Millisecond)
	case d >= time.Millisecond:
		return d.Round(time.Millisecond)
	default:
		return d.Round(10 * time.Microsecond)
	}
}
