package scenario

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wpan36/incident_diag/internal/lab"
	"github.com/wpan36/incident_diag/internal/log"
)

// faultLog records what a scenario did to a service's /fault endpoint.
type faultLog struct {
	mu      sync.Mutex
	posts   []string
	deletes int
}

func (f *faultLog) record(r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method {
	case http.MethodPost:
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		f.posts = append(f.posts, string(body))
	case http.MethodDelete:
		f.deletes++
	}
}

func (f *faultLog) snapshot() ([]string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.posts...), f.deletes
}

// newLab starts the two real services in-process. The scenario tool is only
// worth testing against them: a fake that answers 200 to everything would let a
// broken fault call pass.
func newLab(t *testing.T) (checkoutURL, paymentURL string, faults *faultLog, payment *lab.App) {
	t.Helper()

	faults = &faultLog{}

	paymentApp := lab.New(lab.Options{Service: "payment-service", Logger: log.Discard()})
	lab.NewPayment(paymentApp, lab.PaymentOptions{PoolSize: 4, ProcessorLatency: time.Millisecond})
	paymentSrv := httptest.NewServer(recordFaults(faults, paymentApp.Handler()))
	t.Cleanup(paymentSrv.Close)

	checkoutApp := lab.New(lab.Options{Service: "checkout-service", Logger: log.Discard(),
		LatencyFaultInMiddleware: true})
	lab.NewCheckout(checkoutApp, lab.CheckoutOptions{
		PaymentURL: paymentSrv.URL, PaymentTimeout: 500 * time.Millisecond})
	checkoutSrv := httptest.NewServer(recordFaults(faults, checkoutApp.Handler()))
	t.Cleanup(checkoutSrv.Close)

	return checkoutSrv.URL, paymentSrv.URL, faults, paymentApp
}

func recordFaults(f *faultLog, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fault" {
			f.record(r)
		}
		next.ServeHTTP(w, r)
	})
}

// shortConfig runs the three phases in about a second.
func shortConfig(checkoutURL, paymentURL string, out io.Writer) Config {
	return Config{
		CheckoutURL:    checkoutURL,
		PaymentURL:     paymentURL,
		RPS:            50,
		Baseline:       200 * time.Millisecond,
		Hold:           300 * time.Millisecond,
		Recovery:       200 * time.Millisecond,
		RequestTimeout: 2 * time.Second,
		MaxInFlight:    50,
		Params:         Params{DelayMS: 20, JitterMS: 5, Status: 503, Ratio: 0.5, Workers: 1},
		Out:            out,
	}
}

func TestRunAppliesAndClearsTheFault(t *testing.T) {
	checkoutURL, paymentURL, faults, payment := newLab(t)
	var out bytes.Buffer

	s, err := Find("payment-latency")
	if err != nil {
		t.Fatalf("Find(): %v", err)
	}
	if err := Run(t.Context(), shortConfig(checkoutURL, paymentURL, &out), s); err != nil {
		t.Fatalf("Run(): %v", err)
	}

	posts, deletes := faults.snapshot()
	if len(posts) != 1 || !strings.Contains(posts[0], `"kind":"latency"`) {
		t.Errorf("fault posts = %v, want one latency fault", posts)
	}
	// Cleared before the run as well as after it: a previous run that was
	// killed may have left a fault enabled, and measuring someone else's
	// incident is worse than measuring nothing.
	if deletes < 2 {
		t.Errorf("fault deletes = %d, want the lab cleared before and after the run", deletes)
	}
	if state := payment.Faults().State(); state.Latency.Enabled {
		t.Error("the latency fault is still enabled after the run")
	}

	report := out.String()
	for _, want := range []string{"payment-latency", "baseline", "fault", "recovery", routeCreateOrder, routeGetOrder} {
		if !strings.Contains(report, want) {
			t.Errorf("the report does not mention %q:\n%s", want, report)
		}
	}
}

func TestRunClearsTheFaultAfterCancellation(t *testing.T) {
	checkoutURL, paymentURL, _, payment := newLab(t)
	var out bytes.Buffer

	s, err := Find("payment-latency")
	if err != nil {
		t.Fatalf("Find(): %v", err)
	}
	cfg := shortConfig(checkoutURL, paymentURL, &out)
	cfg.Hold = 30 * time.Second // long enough that the cancellation lands inside it

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, s) }()

	// Wait for the fault to actually be enabled before interrupting, so this
	// tests the cleanup rather than a run that never started.
	deadline := time.Now().Add(10 * time.Second)
	for !payment.Faults().State().Latency.Enabled {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("the latency fault was never enabled")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()

	select {
	case err := <-done:
		if err != nil && !strings.Contains(err.Error(), "context canceled") {
			t.Fatalf("Run(): %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}

	// The guarantee that matters: a Ctrl-C must not leave the lab broken for
	// the next scenario.
	if state := payment.Faults().State(); state.Latency.Enabled {
		t.Error("the latency fault is still enabled after the run was cancelled")
	}
}

func TestBaselineRunInjectsNothing(t *testing.T) {
	checkoutURL, paymentURL, faults, _ := newLab(t)
	var out bytes.Buffer

	s, err := Find("baseline")
	if err != nil {
		t.Fatalf("Find(): %v", err)
	}
	if err := Run(t.Context(), shortConfig(checkoutURL, paymentURL, &out), s); err != nil {
		t.Fatalf("Run(): %v", err)
	}

	if posts, _ := faults.snapshot(); len(posts) != 0 {
		t.Errorf("fault posts = %v, want none for the healthy control", posts)
	}
	// A healthy run has to look healthy, or the negative case proves nothing.
	if !strings.Contains(out.String(), "201×") {
		t.Errorf("the report shows no created orders:\n%s", out.String())
	}
}

func TestGeneratorRespectsTheInFlightBound(t *testing.T) {
	var mu sync.Mutex
	concurrent, peak := 0, 0
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		concurrent++
		if concurrent > peak {
			peak = concurrent
		}
		mu.Unlock()
		time.Sleep(200 * time.Millisecond)
		mu.Lock()
		concurrent--
		mu.Unlock()
		w.WriteHeader(http.StatusGatewayTimeout)
	}))
	defer slow.Close()

	g := &generator{
		client:         &http.Client{Transport: pooledTransport(50)},
		checkoutURL:    slow.URL,
		rps:            200,
		maxInFlight:    5,
		requestTimeout: 2 * time.Second,
	}
	stats := g.run(t.Context(), "fault", 500*time.Millisecond)

	// The bound is what stops the generator from being the thing that falls
	// over, and reaching it has to be reported: the demand under it understates
	// the real load.
	if peak > 5 {
		t.Errorf("peak concurrency = %d, want at most 5", peak)
	}
	if stats.skipped == 0 {
		t.Error("no dispatches were reported as refused, though the bound was reached")
	}
}

func TestGeneratorPacesItself(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		w.WriteHeader(http.StatusGatewayTimeout) // no order is created, so no second call
	}))
	defer fast.Close()

	g := &generator{
		client:         &http.Client{Transport: pooledTransport(50)},
		checkoutURL:    fast.URL,
		rps:            50,
		maxInFlight:    100,
		requestTimeout: time.Second,
	}
	g.run(t.Context(), "baseline", time.Second)

	mu.Lock()
	defer mu.Unlock()
	// Open loop: about rps × duration, not "as many as the server could take".
	// Wide bounds, because this asserts the pacing exists rather than the
	// scheduler's precision.
	if requests < 25 || requests > 75 {
		t.Errorf("sent %d requests in a second at 50 rps, want roughly 50", requests)
	}
}
