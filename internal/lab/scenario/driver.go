package scenario

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// maxHold bounds how long a Driver keeps traffic flowing if nothing stops it.
//
// The generator takes a duration rather than running until cancelled, so this
// is the value passed in. It is far longer than any investigation and exists
// only so that a leaked Driver eventually goes quiet.
const maxHold = 30 * time.Minute

// Driver holds a scenario's fault open and keeps traffic flowing while
// something else — the agent — investigates it.
//
// Run is the other way to drive the lab: fixed baseline, fault and recovery
// phases, ending in a report about the metrics. That shape is wrong for an
// investigation, which has to start after the fault has been visible for a
// while and lasts however long the model takes. So this one has no phases and
// no report; it starts, holds, and stops.
type Driver struct {
	cfg    Config
	client *http.Client

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

// NewDriver builds a driver. Nothing is contacted until Start.
func NewDriver(cfg Config) *Driver {
	return &Driver{cfg: cfg, client: &http.Client{Transport: pooledTransport(cfg.MaxInFlight)}}
}

// ResetMetrics deletes every series Prometheus holds.
//
// It exists because the agent looks back over an hour, and an hour ago the lab
// was broken on purpose for a different reason. Without this, an evaluation
// measures how well the agent tells the current fault from the previous one,
// which is not the question being asked — a run against a healthy system read
// the previous scenario's latency out of the history and reported, correctly,
// that something had been wrong.
//
// It needs --web.enable-admin-api, which deploy/docker-compose.yml sets. A
// Prometheus without it returns 500 and this returns an error saying so,
// rather than quietly leaving the history in place.
func (d *Driver) ResetMetrics(ctx context.Context) error {
	if d.cfg.PrometheusURL == "" {
		return nil
	}
	for _, path := range []string{
		"/api/v1/admin/tsdb/delete_series?match%5B%5D=%7B__name__%3D~%22.%2B%22%7D",
		"/api/v1/admin/tsdb/clean_tombstones",
	} {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.cfg.PrometheusURL+path, nil)
		if err != nil {
			return err
		}
		resp, err := d.client.Do(req)
		if err != nil {
			return fmt.Errorf("scenario: reset the metric history: %w", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
			return fmt.Errorf("scenario: reset the metric history: %s "+
				"(is --web.enable-admin-api set?)", resp.Status)
		}
	}
	return nil
}

// Start clears any leftover fault, injects this scenario's, and begins load.
//
// It returns once the fault is set and traffic is flowing; warm is how long to
// keep going before returning, which is what gives Prometheus enough points
// for the agent's queries to mean anything. A scenario with no fault is the
// healthy control and still gets its load.
//
// ctx bounds the setup calls only. The load runs until Stop.
func (d *Driver) Start(ctx context.Context, s Scenario, warm time.Duration) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cancel != nil {
		return fmt.Errorf("scenario: the driver is already running %s", s.Name)
	}

	if err := waitForHealth(ctx, d.client, d.cfg); err != nil {
		return err
	}
	// A previous run that was killed may have left a fault enabled, and an
	// investigation of someone else's incident proves nothing.
	if err := clearFaults(ctx, d.client, d.cfg); err != nil {
		return err
	}
	if s.Fault != nil {
		call := s.Fault(d.cfg.Params)
		if err := postFault(ctx, d.client, d.cfg, call.target, call.body); err != nil {
			return err
		}
	}

	gen := &generator{
		client:         d.client,
		checkoutURL:    d.cfg.CheckoutURL,
		rps:            d.cfg.RPS,
		maxInFlight:    d.cfg.MaxInFlight,
		requestTimeout: d.cfg.RequestTimeout,
	}

	// The load goroutine is owned by this driver and ends when Stop cancels
	// loadCtx, or when maxHold expires, whichever comes first. It deliberately
	// does not take the caller's ctx: a cancelled investigation still has to
	// go through Stop, which is what clears the fault.
	loadCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	d.cancel = cancel
	d.done = make(chan struct{})
	go func() {
		defer close(d.done)
		gen.run(loadCtx, s.Name, maxHold)
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(warm):
	}
	return nil
}

// Stop ends the load and clears the fault.
//
// It is safe to call twice and safe to call after Start failed, so a deferred
// Stop is enough to leave the lab healthy on every exit path.
func (d *Driver) Stop(ctx context.Context) error {
	d.mu.Lock()
	cancel, done := d.cancel, d.done
	d.cancel, d.done = nil, nil
	d.mu.Unlock()

	if cancel != nil {
		cancel()
		<-done
	}
	// Even when there was nothing to cancel: a fault outliving the process
	// that set it is the one failure that breaks every run after it.
	return clearFaults(ctx, d.client, d.cfg)
}
