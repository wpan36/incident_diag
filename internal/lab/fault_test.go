package lab

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/wpan36/incident_diag/internal/log"
)

func newControllerForTest() *Controller { return NewController(log.Discard()) }

// waitForGoroutines fails unless the goroutine count falls back to want.
//
// It polls rather than asserting immediately because an earlier test's
// httptest connections may still be winding down. A spinning fault worker never
// exits on its own, so a real leak still fails here.
func waitForGoroutines(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := runtime.NumGoroutine()
		if got <= want {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("goroutines = %d, want %d", got, want)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestLatencyFaultIsOffUntilEnabled(t *testing.T) {
	c := newControllerForTest()
	if d := c.Delay(); d != 0 {
		t.Errorf("Delay() = %v on a fresh controller, want 0", d)
	}

	if err := c.SetLatency(LatencyFault{Enabled: true, DelayMS: 100}); err != nil {
		t.Fatalf("SetLatency(): %v", err)
	}
	if d := c.Delay(); d != 100*time.Millisecond {
		t.Errorf("Delay() = %v, want 100ms", d)
	}

	if err := c.SetLatency(LatencyFault{Enabled: false, DelayMS: 100}); err != nil {
		t.Fatalf("SetLatency(): %v", err)
	}
	if d := c.Delay(); d != 0 {
		t.Errorf("Delay() = %v after disabling, want 0", d)
	}
}

func TestLatencyJitterStaysWithinBounds(t *testing.T) {
	c := newControllerForTest()
	if err := c.SetLatency(LatencyFault{Enabled: true, DelayMS: 100, JitterMS: 40}); err != nil {
		t.Fatalf("SetLatency(): %v", err)
	}
	for range 200 {
		d := c.Delay()
		if d < 60*time.Millisecond || d > 140*time.Millisecond {
			t.Fatalf("Delay() = %v, want it within 100ms ± 40ms", d)
		}
	}
}

func TestLatencyJitterLargerThanTheDelayNeverGoesNegative(t *testing.T) {
	c := newControllerForTest()
	// A time.Duration is signed, so an unclamped negative would be a timer that
	// fires immediately rather than an obvious failure.
	if err := c.SetLatency(LatencyFault{Enabled: true, DelayMS: 10, JitterMS: 100}); err != nil {
		t.Fatalf("SetLatency(): %v", err)
	}
	for range 200 {
		if d := c.Delay(); d < 0 {
			t.Fatalf("Delay() = %v, want at least 0", d)
		}
	}
}

func TestErrorFaultRatio(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ratio float64
		want  int // how many of 200 calls should fail
	}{
		{"never", 0, 0},
		{"always", 1, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newControllerForTest()
			if err := c.SetError(ErrorFault{Enabled: true, Status: 503, Ratio: tc.ratio}); err != nil {
				t.Fatalf("SetError(): %v", err)
			}
			failed := 0
			for range 200 {
				if status, ok := c.ErrorStatus(); ok {
					if status != 503 {
						t.Fatalf("ErrorStatus() = %d, want 503", status)
					}
					failed++
				}
			}
			if failed != tc.want {
				t.Errorf("%d of 200 calls failed, want %d", failed, tc.want)
			}
		})
	}
}

func TestErrorFaultRatioIsRoughlyRespected(t *testing.T) {
	c := newControllerForTest()
	if err := c.SetError(ErrorFault{Enabled: true, Status: 500, Ratio: 0.25}); err != nil {
		t.Fatalf("SetError(): %v", err)
	}
	const n = 4000
	failed := 0
	for range n {
		if _, ok := c.ErrorStatus(); ok {
			failed++
		}
	}
	// Wide bounds on purpose: this asserts that the ratio is applied at all,
	// not that the random source is uniform.
	if failed < n*15/100 || failed > n*35/100 {
		t.Errorf("%d of %d calls failed, want roughly %d", failed, n, n/4)
	}
}

func TestFaultValidation(t *testing.T) {
	c := newControllerForTest()
	for _, tc := range []struct {
		name string
		set  func() error
	}{
		{"negative delay", func() error { return c.SetLatency(LatencyFault{Enabled: true, DelayMS: -1}) }},
		{"negative jitter", func() error { return c.SetLatency(LatencyFault{Enabled: true, JitterMS: -1}) }},
		{"status below 400", func() error { return c.SetError(ErrorFault{Enabled: true, Status: 200, Ratio: 1}) }},
		{"status above 599", func() error { return c.SetError(ErrorFault{Enabled: true, Status: 600, Ratio: 1}) }},
		{"ratio above one", func() error { return c.SetError(ErrorFault{Enabled: true, Status: 500, Ratio: 1.5}) }},
		{"no workers", func() error { return c.SetCPU(CPUFault{Enabled: true, Workers: 0}) }},
		{"too many workers", func() error { return c.SetCPU(CPUFault{Enabled: true, Workers: maxCPUWorkers + 1}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.set(); err == nil {
				t.Error("the fault was accepted, want a validation error")
			}
		})
	}
}

func TestCPUFaultGoroutinesExit(t *testing.T) {
	c := newControllerForTest()
	before := runtime.NumGoroutine()

	if err := c.SetCPU(CPUFault{Enabled: true, Workers: 2}); err != nil {
		t.Fatalf("SetCPU(): %v", err)
	}
	if got := runtime.NumGoroutine(); got < before+2 {
		t.Errorf("goroutines = %d, want at least %d with two spinning", got, before+2)
	}

	// SetCPU waits for the goroutines it stops, so the count is exact
	// immediately afterwards rather than eventually. Without that guarantee a
	// scenario that disables a fault would leave a core busy for a while.
	if err := c.SetCPU(CPUFault{Enabled: false}); err != nil {
		t.Fatalf("SetCPU(): %v", err)
	}
	waitForGoroutines(t, before)
}

func TestCPUFaultIsStoppedByClose(t *testing.T) {
	c := newControllerForTest()
	before := runtime.NumGoroutine()

	if err := c.SetCPU(CPUFault{Enabled: true, Workers: 1}); err != nil {
		t.Fatalf("SetCPU(): %v", err)
	}
	// The second exit path: a process shutting down must not depend on anyone
	// having called DELETE /fault first.
	if err := c.Close(t.Context()); err != nil {
		t.Fatalf("Close(): %v", err)
	}
	waitForGoroutines(t, before)
	if c.State().CPU.Enabled {
		t.Error("State() still reports the cpu fault as enabled after Close")
	}
}

func TestEnablingTheCPUFaultTwiceDoesNotAccumulateGoroutines(t *testing.T) {
	c := newControllerForTest()
	defer c.Close(t.Context())
	before := runtime.NumGoroutine()

	for range 3 {
		if err := c.SetCPU(CPUFault{Enabled: true, Workers: 1}); err != nil {
			t.Fatalf("SetCPU(): %v", err)
		}
	}
	waitForGoroutines(t, before+1)
}

func TestClearDisablesEverything(t *testing.T) {
	c := newControllerForTest()
	if err := c.SetLatency(LatencyFault{Enabled: true, DelayMS: 50}); err != nil {
		t.Fatalf("SetLatency(): %v", err)
	}
	if err := c.SetError(ErrorFault{Enabled: true, Status: 503, Ratio: 1}); err != nil {
		t.Fatalf("SetError(): %v", err)
	}
	if err := c.SetCPU(CPUFault{Enabled: true, Workers: 1}); err != nil {
		t.Fatalf("SetCPU(): %v", err)
	}

	c.Clear()

	state := c.State()
	if state.Latency.Enabled || state.Error.Enabled || state.CPU.Enabled {
		t.Errorf("State() = %+v after Clear, want everything disabled", state)
	}
	if d := c.Delay(); d != 0 {
		t.Errorf("Delay() = %v after Clear", d)
	}
	if _, ok := c.ErrorStatus(); ok {
		t.Error("ErrorStatus() still fails requests after Clear")
	}
}

// post sends one /fault request and returns the response.
func post(t *testing.T, h http.Handler, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/fault", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestFaultEndpoint(t *testing.T) {
	c := newControllerForTest()
	defer c.Close(t.Context())
	h := c.Handler()

	// One endpoint, dispatched on kind, so a scenario script carries one URL.
	if rec := post(t, h, http.MethodPost, `{"kind":"latency","enabled":true,"delay_ms":2500,"jitter_ms":250}`); rec.Code != http.StatusOK {
		t.Fatalf("POST latency = %d: %s", rec.Code, rec.Body)
	}
	if got := c.State().Latency; !got.Enabled || got.DelayMS != 2500 || got.JitterMS != 250 {
		t.Errorf("latency = %+v", got)
	}

	if rec := post(t, h, http.MethodPost, `{"kind":"error","enabled":true,"status":503,"ratio":0.2}`); rec.Code != http.StatusOK {
		t.Fatalf("POST error = %d: %s", rec.Code, rec.Body)
	}
	if rec := post(t, h, http.MethodPost, `{"kind":"cpu","enabled":true,"workers":1}`); rec.Code != http.StatusOK {
		t.Fatalf("POST cpu = %d: %s", rec.Code, rec.Body)
	}

	rec := post(t, h, http.MethodGet, "")
	var state State
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatalf("GET /fault is not JSON: %v", err)
	}
	if !state.Latency.Enabled || !state.Error.Enabled || !state.CPU.Enabled {
		t.Errorf("GET /fault = %+v, want every kind enabled", state)
	}

	if rec := post(t, h, http.MethodDelete, ""); rec.Code != http.StatusOK {
		t.Fatalf("DELETE = %d: %s", rec.Code, rec.Body)
	}
	if state := c.State(); state.Latency.Enabled || state.Error.Enabled || state.CPU.Enabled {
		t.Errorf("State() = %+v after DELETE", state)
	}
}

func TestFreshControllerNamesItsKinds(t *testing.T) {
	// GET /fault on a process nobody has touched is the state a scenario or an
	// operator reads first, and three empty kind fields there would make the
	// field unusable exactly when it is most likely to be checked.
	state := newControllerForTest().State()
	if state.Latency.Kind != KindLatency || state.Error.Kind != KindError || state.CPU.Kind != KindCPU {
		t.Errorf("State() = %+v on a fresh controller, want every kind named", state)
	}
	// And the kind is the controller's, not the caller's: a Set from Go code
	// passes no kind at all.
	c := newControllerForTest()
	if err := c.SetLatency(LatencyFault{Enabled: true, DelayMS: 10}); err != nil {
		t.Fatalf("SetLatency(): %v", err)
	}
	if got := c.State().Latency.Kind; got != KindLatency {
		t.Errorf("Latency.Kind = %q after a Set that passed none, want %q", got, KindLatency)
	}
}

func TestFaultEndpointRejectsBadRequests(t *testing.T) {
	h := newControllerForTest().Handler()
	for _, tc := range []struct {
		name string
		body string
	}{
		{"empty body", ``},
		{"not json", `nope`},
		{"unknown kind", `{"kind":"disk","enabled":true}`},
		{"missing kind", `{"enabled":true,"delay_ms":100}`},
		{"invalid value", `{"kind":"error","enabled":true,"status":200,"ratio":1}`},
		// A misspelled field is the failure a lab must not have: accepted, it
		// would answer 200 and report a fault that is enabled and does nothing.
		{"misspelled field", `{"kind":"latency","enabled":true,"delayMs":2500}`},
		{"another kind's field", `{"kind":"cpu","enabled":true,"workers":1,"delay_ms":100}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := post(t, h, http.MethodPost, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("POST %s = %d, want 400", tc.body, rec.Code)
			}
		})
	}
}
