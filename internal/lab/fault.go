package lab

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Fault kinds, as they appear in the "kind" field of a /fault request.
const (
	KindLatency = "latency"
	KindError   = "error"
	KindCPU     = "cpu"
)

// MaxCPUWorkers caps the cpu fault. The point is to saturate a container
// limited to one CPU, and an unbounded worker count in a lab endpoint with no
// authentication is a foot-gun rather than a feature.
//
// Exported because internal/lab/scenario's payment-cpu case asks for the
// maximum by name rather than repeating the number.
const MaxCPUWorkers = 32

// LatencyFault delays each request. On checkout-service it is applied before
// the handler runs; on payment-service it is added to the simulated processor
// call, inside the connection pool, which is what makes the pool fill.
type LatencyFault struct {
	Kind     string `json:"kind"`
	Enabled  bool   `json:"enabled"`
	DelayMS  int    `json:"delay_ms"`
	JitterMS int    `json:"jitter_ms"`
}

// ErrorFault answers a share of requests with a status before any work is
// done.
type ErrorFault struct {
	Kind    string  `json:"kind"`
	Enabled bool    `json:"enabled"`
	Status  int     `json:"status"`
	Ratio   float64 `json:"ratio"`
}

// CPUFault spins goroutines until it is disabled or the process shuts down.
type CPUFault struct {
	Kind    string `json:"kind"`
	Enabled bool   `json:"enabled"`
	Workers int    `json:"workers"`
}

// State is every fault's current setting, as returned by GET /fault.
type State struct {
	Latency LatencyFault `json:"latency"`
	Error   ErrorFault   `json:"error"`
	CPU     CPUFault     `json:"cpu"`
}

// Controller holds the injected faults for one process.
//
// State is in memory and per process, so a restart is a reset. That is the
// expected way to clean up after a scenario, and it is also why a restarted
// container is a silently healthy one.
type Controller struct {
	logger *slog.Logger

	mu        sync.Mutex
	latency   LatencyFault
	failure   ErrorFault
	cpu       CPUFault
	cpuCancel context.CancelFunc
	cpuWG     sync.WaitGroup
}

// NewController returns a controller with no faults enabled.
func NewController(logger *slog.Logger) *Controller {
	return &Controller{
		logger: logger,
		// The kinds are set here as well as in the setters, so GET /fault on a
		// process nobody has touched yet names its three faults rather than
		// answering with three empty strings.
		latency: LatencyFault{Kind: KindLatency},
		failure: ErrorFault{Kind: KindError},
		cpu:     CPUFault{Kind: KindCPU},
	}
}

// State returns every fault's current setting.
func (c *Controller) State() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return State{Latency: c.latency, Error: c.failure, CPU: c.cpu}
}

// Delay returns how long this request should be held, or zero when the latency
// fault is disabled.
//
// The jitter is symmetric around the delay and clamped at zero, so a latency
// fault produces a distribution rather than a single spike that every
// percentile lands on.
func (c *Controller) Delay() time.Duration {
	c.mu.Lock()
	f := c.latency
	c.mu.Unlock()

	if !f.Enabled || f.DelayMS <= 0 {
		return 0
	}
	ms := float64(f.DelayMS)
	if f.JitterMS > 0 {
		ms += (rand.Float64()*2 - 1) * float64(f.JitterMS)
	}
	return time.Duration(math.Max(0, ms)) * time.Millisecond
}

// ErrorStatus reports whether this request should be failed immediately, and
// with which status.
func (c *Controller) ErrorStatus() (int, bool) {
	c.mu.Lock()
	f := c.failure
	c.mu.Unlock()

	if !f.Enabled || f.Ratio <= 0 {
		return 0, false
	}
	if f.Ratio < 1 && rand.Float64() >= f.Ratio {
		return 0, false
	}
	return f.Status, true
}

// SetLatency enables or disables the latency fault.
func (c *Controller) SetLatency(f LatencyFault) error {
	if f.DelayMS < 0 || f.JitterMS < 0 {
		return fmt.Errorf("delay_ms and jitter_ms must not be negative")
	}
	// Forced rather than taken from the caller: the kind is which field of the
	// controller this is, not something a request body gets to decide.
	f.Kind = KindLatency
	c.mu.Lock()
	c.latency = f
	c.mu.Unlock()
	c.log(KindLatency, f.Enabled, "delay_ms", f.DelayMS, "jitter_ms", f.JitterMS)
	return nil
}

// SetError enables or disables the error fault.
func (c *Controller) SetError(f ErrorFault) error {
	if f.Enabled {
		if f.Status < 400 || f.Status > 599 {
			return fmt.Errorf("status must be between 400 and 599, got %d", f.Status)
		}
		if f.Ratio < 0 || f.Ratio > 1 {
			return fmt.Errorf("ratio must be between 0 and 1, got %v", f.Ratio)
		}
	}
	f.Kind = KindError
	c.mu.Lock()
	c.failure = f
	c.mu.Unlock()
	c.log(KindError, f.Enabled, "status", f.Status, "ratio", f.Ratio)
	return nil
}

// SetCPU starts or stops the spinning goroutines.
func (c *Controller) SetCPU(f CPUFault) error {
	if f.Enabled && (f.Workers < 1 || f.Workers > MaxCPUWorkers) {
		return fmt.Errorf("workers must be between 1 and %d, got %d", MaxCPUWorkers, f.Workers)
	}
	f.Kind = KindCPU
	c.mu.Lock()
	// Stopping first makes this idempotent: enabling twice replaces the
	// goroutines rather than accumulating two sets nothing holds a handle to.
	c.stopCPULocked()
	c.cpu = f
	if f.Enabled {
		c.startCPULocked(f.Workers)
	}
	c.mu.Unlock()
	c.log(KindCPU, f.Enabled, "workers", f.Workers)
	return nil
}

// Clear disables every fault. It is what DELETE /fault does, and what a
// scenario script calls on its way out.
func (c *Controller) Clear() {
	c.mu.Lock()
	c.stopCPULocked()
	c.latency = LatencyFault{Kind: KindLatency}
	c.failure = ErrorFault{Kind: KindError}
	c.cpu = CPUFault{Kind: KindCPU}
	c.mu.Unlock()
	c.logger.Info("faults cleared")
}

// Close stops the cpu fault's goroutines. It is registered with the process's
// shutdown group, so those goroutines have an exit condition that does not
// depend on anyone calling DELETE /fault.
func (c *Controller) Close(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopCPULocked()
	c.cpu = CPUFault{Kind: KindCPU}
	return nil
}

// startCPULocked must be called with c.mu held.
func (c *Controller) startCPULocked(workers int) {
	// Background, not a request context: the fault outlives the call that
	// enabled it. Both exit paths — SetCPU and Close — cancel it.
	ctx, cancel := context.WithCancel(context.Background())
	c.cpuCancel = cancel
	for range workers {
		c.cpuWG.Add(1)
		go func() {
			defer c.cpuWG.Done()
			burnCPU(ctx)
		}()
	}
}

// stopCPULocked must be called with c.mu held. Waiting here is safe because the
// spinning goroutines never take the lock.
func (c *Controller) stopCPULocked() {
	if c.cpuCancel == nil {
		return
	}
	c.cpuCancel()
	c.cpuWG.Wait()
	c.cpuCancel = nil
}

func (c *Controller) log(kind string, enabled bool, args ...any) {
	// A fault being turned on is the most significant thing that happens to
	// these services, and WARN is what makes it visible to read_service_logs'
	// min_level filter.
	level := slog.LevelWarn
	msg := "fault enabled"
	if !enabled {
		level, msg = slog.LevelInfo, "fault disabled"
	}
	c.logger.Log(context.Background(), level, msg, append([]any{"kind", kind}, args...)...)
}

// cpuSink keeps the compiler from deciding the spin loop computes nothing.
var cpuSink atomic.Uint64

// burnCPU spins until ctx is cancelled. The inner loop is long enough that the
// cancellation check is not most of the work, and short enough that stopping
// the fault is immediate.
func burnCPU(ctx context.Context) {
	x := 1.000001
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		for range 200_000 {
			x = math.Sqrt(x*3 + 1)
		}
		cpuSink.Store(math.Float64bits(x))
	}
}

// Handler serves /fault: POST to set one kind, GET to read every kind, DELETE
// to clear them all.
//
// One endpoint dispatching on "kind" rather than three endpoints, so a scenario
// script carries one URL. The payload's fields vary by kind, so decoding is two
// steps: the kind first, then the rest.
func (c *Controller) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, c.State())
		case http.MethodDelete:
			c.Clear()
			writeJSON(w, http.StatusOK, c.State())
		case http.MethodPost:
			c.set(w, r)
		default:
			writeError(w, http.StatusMethodNotAllowed, "use GET, POST or DELETE")
		}
	}
}

// decodeFault is the second of the two decoding steps: the body again, now
// into the type the kind selected.
//
// Unknown fields are refused, for the reason decodeJSON gives. A misspelled
// "delayMs" would otherwise answer 200 and leave GET /fault reporting a
// latency fault that is enabled and adds nothing, which is the one failure
// mode a lab must not have.
func decodeFault(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid fault body: %w", err)
	}
	return nil
}

func (c *Controller) set(w http.ResponseWriter, r *http.Request) {
	body := http.MaxBytesReader(w, r.Body, 4<<10)

	var envelope struct {
		Kind string `json:"kind"`
	}
	raw, err := decodeRaw(body, &envelope)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	var applyErr error
	switch envelope.Kind {
	case KindLatency:
		var f LatencyFault
		if applyErr = decodeFault(raw, &f); applyErr == nil {
			applyErr = c.SetLatency(f)
		}
	case KindError:
		var f ErrorFault
		if applyErr = decodeFault(raw, &f); applyErr == nil {
			applyErr = c.SetError(f)
		}
	case KindCPU:
		var f CPUFault
		if applyErr = decodeFault(raw, &f); applyErr == nil {
			applyErr = c.SetCPU(f)
		}
	default:
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("kind must be one of %s, %s, %s", KindLatency, KindError, KindCPU))
		return
	}
	if applyErr != nil {
		writeError(w, http.StatusBadRequest, applyErr.Error())
		return
	}
	writeJSON(w, http.StatusOK, c.State())
}
