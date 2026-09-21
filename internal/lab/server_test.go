package lab

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wpan36/incident_diag/internal/log"
)

// newTestApp builds a service with one business route, so the middleware can be
// tested without either real service's handlers.
func newTestApp(latencyInMiddleware bool) *App {
	app := New(Options{Service: "test-service", Logger: log.Discard(),
		LatencyFaultInMiddleware: latencyInMiddleware})
	app.Handle(Route{Method: http.MethodGet, Path: "/thing", Kind: RouteBusiness,
		Handler: func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, map[string]string{"ok": "yes"})
		}})
	app.Handle(Route{Method: http.MethodGet, Path: "/broken", Kind: RouteBusiness,
		Handler: func(w http.ResponseWriter, _ *http.Request) {
			writeError(w, http.StatusInternalServerError, "deliberate")
		}})
	return app
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestHealthIsMeasuredAndMetricsIsNot(t *testing.T) {
	app := newTestApp(false)
	h := app.Handler()

	if rec := get(t, h, "/health"); rec.Code != http.StatusOK {
		t.Fatalf("GET /health = %d", rec.Code)
	}
	get(t, h, "/metrics")

	// /health is in the histogram because the CPU runbook's central signal is
	// that saturation slows every endpoint, including the ones that call
	// nothing downstream.
	if got := requestCount(t, app, "/health", "200"); got != 1 {
		t.Errorf("/health observations = %d, want 1", got)
	}
	// /metrics is not, because counting the scraper's own traffic would put a
	// request every fifteen seconds into a service's rate.
	if got := requestCount(t, app, "/metrics", ""); got != 0 {
		t.Errorf("/metrics observations = %d, want 0", got)
	}
}

func TestErrorFaultSkipsHealthAndFault(t *testing.T) {
	app := newTestApp(false)
	h := app.Handler()
	if err := app.Faults().SetError(ErrorFault{Enabled: true, Status: 503, Ratio: 1}); err != nil {
		t.Fatalf("SetError(): %v", err)
	}

	if rec := get(t, h, "/thing"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("GET /thing = %d, want 503 with the error fault at ratio 1", rec.Code)
	}
	// The fault endpoint has to keep working when the fault fails everything,
	// or a scenario could enable a fault it cannot turn off.
	if rec := get(t, h, "/fault"); rec.Code != http.StatusOK {
		t.Errorf("GET /fault = %d while faulted, want 200", rec.Code)
	}
	// A failing /health would take the container out of service instead of
	// making it look unhealthy in the metrics, which is not the fault being
	// asked for.
	if rec := get(t, h, "/health"); rec.Code != http.StatusOK {
		t.Errorf("GET /health = %d while faulted, want 200", rec.Code)
	}
}

func TestErrorFaultRunsBeforeTheHandler(t *testing.T) {
	handlerRan := false
	app := New(Options{Service: "test-service", Logger: log.Discard()})
	app.Handle(Route{Method: http.MethodGet, Path: "/thing", Kind: RouteBusiness,
		Handler: func(w http.ResponseWriter, _ *http.Request) {
			handlerRan = true
			w.WriteHeader(http.StatusOK)
		}})
	if err := app.Faults().SetError(ErrorFault{Enabled: true, Status: 500, Ratio: 1}); err != nil {
		t.Fatalf("SetError(): %v", err)
	}

	get(t, app.Handler(), "/thing")

	// "before doing any work" is the whole of the error fault: nothing
	// downstream should see a request that was failed here.
	if handlerRan {
		t.Error("the handler ran despite the error fault")
	}
}

func TestLatencyFaultInMiddlewareAppliesToBusinessRoutesOnly(t *testing.T) {
	app := newTestApp(true)
	h := app.Handler()
	if err := app.Faults().SetLatency(LatencyFault{Enabled: true, DelayMS: 120}); err != nil {
		t.Fatalf("SetLatency(): %v", err)
	}

	start := time.Now()
	get(t, h, "/thing")
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Errorf("GET /thing took %v, want the injected 120ms", elapsed)
	}

	start = time.Now()
	get(t, h, "/fault")
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("GET /fault took %v, want the fault endpoint to stay fast", elapsed)
	}
}

func TestLatencyFaultIsNotAppliedInTheMiddlewareForPayment(t *testing.T) {
	app := newTestApp(false)
	if err := app.Faults().SetLatency(LatencyFault{Enabled: true, DelayMS: 300}); err != nil {
		t.Fatalf("SetLatency(): %v", err)
	}

	// payment-service adds the delay inside the processor call instead, while a
	// pool connection is held. Applying it here as well would double it and
	// break the relationship between the fault and the in-use ratio.
	start := time.Now()
	get(t, app.Handler(), "/thing")
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("GET /thing took %v, want no middleware delay", elapsed)
	}
}

func TestStatusAndRouteLabels(t *testing.T) {
	app := newTestApp(false)
	h := app.Handler()

	get(t, h, "/thing")
	get(t, h, "/broken")

	if got := requestCount(t, app, "/thing", "200"); got != 1 {
		t.Errorf("/thing 200 observations = %d, want 1", got)
	}
	// A handler that answered 500 must not be recorded as the 200 the recorder
	// starts at.
	if got := requestCount(t, app, "/broken", "500"); got != 1 {
		t.Errorf("/broken 500 observations = %d, want 1", got)
	}
}

func TestRunStopsOnContextCancellation(t *testing.T) {
	app := newTestApp(false)
	if err := app.Faults().SetCPU(CPUFault{Enabled: true, Workers: 1}); err != nil {
		t.Fatalf("SetCPU(): %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx, "127.0.0.1:0") }()

	// Give the listener a moment to bind before asking it to stop.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run(): %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}

	// Run closes the fault controller, so the spinning goroutine has an exit
	// condition that does not depend on anyone calling DELETE /fault.
	if app.Faults().State().CPU.Enabled {
		t.Error("the cpu fault is still enabled after Run returned")
	}
}
