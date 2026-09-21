package lab

import (
	"testing"
	"time"

	"github.com/wpan36/incident_diag/internal/log"
)

// newCheckoutForTest builds checkout-service with a dependency that never
// answers, which is all most tests need from it.
func newCheckoutForTest(t *testing.T, opts CheckoutOptions) *App {
	t.Helper()
	if opts.PaymentTimeout == 0 {
		opts.PaymentTimeout = 2 * time.Second
	}
	app := New(Options{Service: "checkout-service", Logger: log.Discard(), LatencyFaultInMiddleware: true})
	NewCheckout(app, opts)
	return app
}

func newPaymentForTest(t *testing.T, opts PaymentOptions) (*App, *Payment) {
	t.Helper()
	if opts.PoolSize == 0 {
		opts.PoolSize = 4
	}
	app := New(Options{Service: "payment-service", Logger: log.Discard()})
	return app, NewPayment(app, opts)
}

// gatheredNames returns every metric family the app exports.
func gatheredNames(t *testing.T, app *App) map[string]bool {
	t.Helper()
	families, err := app.Gather()
	if err != nil {
		t.Fatalf("Gather(): %v", err)
	}
	names := make(map[string]bool, len(families))
	for _, f := range families {
		names[f.GetName()] = true
	}
	return names
}

// TestRegisteredMetricNames is the test the whole corpus depends on. Each name
// below appears in a runbook in testdata/knowledge as a step a responder is
// told to take, so a rename in either direction has to be a test failure rather
// than a diagnostic step that silently queries nothing.
func TestRegisteredMetricNames(t *testing.T) {
	// One request each first. http_request_duration_seconds is a labelled
	// vector, so it has no series until something is observed — unlike the pool
	// gauges, which a responder has to be able to read on an idle service and
	// which TestPoolSizeIsPublishedBeforeAnyTraffic covers.
	checkout := newCheckoutForTest(t, CheckoutOptions{PaymentURL: "http://payment.invalid"})
	payment, _ := newPaymentForTest(t, PaymentOptions{})
	get(t, checkout.Handler(), "/health")
	get(t, payment.Handler(), "/health")

	for _, tc := range []struct {
		service string
		names   map[string]bool
		want    []string
		absent  []string
	}{
		{
			service: "checkout-service",
			names:   gatheredNames(t, checkout),
			want:    []string{MetricRequestDuration, MetricClientLatency, MetricClientTimeouts},
			absent:  []string{MetricPoolSize, MetricPoolInUse, MetricPoolWait, MetricProcessorLatency},
		},
		{
			service: "payment-service",
			names:   gatheredNames(t, payment),
			want: []string{MetricRequestDuration, MetricPoolSize, MetricPoolInUse,
				MetricPoolWait, MetricProcessorLatency},
			absent: []string{MetricClientLatency, MetricClientTimeouts},
		},
	} {
		t.Run(tc.service, func(t *testing.T) {
			for _, name := range tc.want {
				if !tc.names[name] {
					t.Errorf("%s does not export %s", tc.service, name)
				}
			}
			// Asserted as absences too: the other service's metrics registered
			// here would publish a number nobody measured.
			for _, name := range tc.absent {
				if tc.names[name] {
					t.Errorf("%s exports %s, which belongs to the other service", tc.service, name)
				}
			}
		})
	}
}

func TestPoolSizeIsPublishedBeforeAnyTraffic(t *testing.T) {
	app, _ := newPaymentForTest(t, PaymentOptions{PoolSize: 7})

	// payment_pool_in_use / payment_pool_size is the first query the latency
	// runbook asks for, so the denominator has to exist on an idle service.
	if got := gaugeValue(t, app, MetricPoolSize); got != 7 {
		t.Errorf("%s = %v, want 7", MetricPoolSize, got)
	}
	if got := gaugeValue(t, app, MetricPoolInUse); got != 0 {
		t.Errorf("%s = %v, want 0", MetricPoolInUse, got)
	}
}

// gaugeValue reads a single unlabelled gauge.
func gaugeValue(t *testing.T, app *App, name string) float64 {
	t.Helper()
	families, err := app.Gather()
	if err != nil {
		t.Fatalf("Gather(): %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		if len(f.GetMetric()) != 1 {
			t.Fatalf("%s has %d series, want 1", name, len(f.GetMetric()))
		}
		return f.GetMetric()[0].GetGauge().GetValue()
	}
	t.Fatalf("%s is not exported", name)
	return 0
}

// histogramCount returns how many observations an unlabelled histogram holds.
func histogramCount(t *testing.T, app *App, name string) uint64 {
	t.Helper()
	families, err := app.Gather()
	if err != nil {
		t.Fatalf("Gather(): %v", err)
	}
	for _, f := range families {
		if f.GetName() == name && len(f.GetMetric()) == 1 {
			return f.GetMetric()[0].GetHistogram().GetSampleCount()
		}
	}
	t.Fatalf("%s is not exported as a single series", name)
	return 0
}

// counterValue returns an unlabelled counter's value.
func counterValue(t *testing.T, app *App, name string) float64 {
	t.Helper()
	families, err := app.Gather()
	if err != nil {
		t.Fatalf("Gather(): %v", err)
	}
	for _, f := range families {
		if f.GetName() == name && len(f.GetMetric()) == 1 {
			return f.GetMetric()[0].GetCounter().GetValue()
		}
	}
	t.Fatalf("%s is not exported as a single series", name)
	return 0
}

// requestCount returns how many requests were observed for one route and
// status, across every method.
func requestCount(t *testing.T, app *App, route, status string) uint64 {
	t.Helper()
	families, err := app.Gather()
	if err != nil {
		t.Fatalf("Gather(): %v", err)
	}
	var total uint64
	for _, f := range families {
		if f.GetName() != MetricRequestDuration {
			continue
		}
		for _, m := range f.GetMetric() {
			var gotRoute, gotStatus string
			for _, l := range m.GetLabel() {
				switch l.GetName() {
				case "route":
					gotRoute = l.GetValue()
				case "status":
					gotStatus = l.GetValue()
				}
			}
			if gotRoute == route && (status == "" || gotStatus == status) {
				total += m.GetHistogram().GetSampleCount()
			}
		}
	}
	return total
}
