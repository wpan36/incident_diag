package lab

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func newPoolForTest(size int) (*Pool, *prometheus.Registry) {
	reg := prometheus.NewRegistry()
	return NewPool(size, newPaymentMetrics(reg)), reg
}

func TestPoolHandsOutItsConnections(t *testing.T) {
	p, _ := newPoolForTest(2)

	releaseFirst, waited, err := p.Acquire(t.Context())
	if err != nil {
		t.Fatalf("Acquire(): %v", err)
	}
	if waited > 50*time.Millisecond {
		t.Errorf("waited %v for an idle pool", waited)
	}
	releaseSecond, _, err := p.Acquire(t.Context())
	if err != nil {
		t.Fatalf("second Acquire(): %v", err)
	}

	// The third caller has to wait, and a deadline is the only thing that ends
	// that wait. This is what answers 503 in the handler.
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, _, err := p.Acquire(ctx); !errors.Is(err, ErrPoolTimeout) {
		t.Errorf("Acquire() on a full pool = %v, want ErrPoolTimeout", err)
	}

	releaseFirst()
	// Once a connection is back, the next caller gets it.
	third, _, err := p.Acquire(t.Context())
	if err != nil {
		t.Fatalf("Acquire() after a release: %v", err)
	}
	third()
	releaseSecond()
}

func TestPoolReleaseIsIdempotent(t *testing.T) {
	p, _ := newPoolForTest(1)

	release, _, err := p.Acquire(t.Context())
	if err != nil {
		t.Fatalf("Acquire(): %v", err)
	}
	// A handler with both a defer and an early release would otherwise return
	// a connection it no longer holds, and the pool would hand out more
	// connections than its size.
	release()
	release()

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	second, _, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire() after a double release: %v", err)
	}
	second()
}

func TestPoolTracksInUse(t *testing.T) {
	p, reg := newPoolForTest(3)

	release, _, err := p.Acquire(t.Context())
	if err != nil {
		t.Fatalf("Acquire(): %v", err)
	}
	if got := gaugeFrom(t, reg, MetricPoolInUse); got != 1 {
		t.Errorf("%s = %v while one connection is held, want 1", MetricPoolInUse, got)
	}
	release()
	if got := gaugeFrom(t, reg, MetricPoolInUse); got != 0 {
		t.Errorf("%s = %v after the release, want 0", MetricPoolInUse, got)
	}
}

func TestPoolObservesTheWaitEvenWhenTheCallerGivesUp(t *testing.T) {
	p, reg := newPoolForTest(1)

	release, _, err := p.Acquire(t.Context())
	if err != nil {
		t.Fatalf("Acquire(): %v", err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if _, _, err := p.Acquire(ctx); err == nil {
		t.Fatal("Acquire() on a full pool succeeded")
	}

	// The pool runbook says to read the wait time before the in-use ratio. A
	// caller that queued and then gave up is exactly the sample that matters,
	// and dropping it would make a saturated pool look like a healthy one.
	if got := histogramCountFrom(t, reg, MetricPoolWait); got != 2 {
		t.Errorf("%s has %d observations, want 2 (one success, one abandoned wait)", MetricPoolWait, got)
	}
}

func gaugeFrom(t *testing.T, g prometheus.Gatherer, name string) float64 {
	t.Helper()
	families, err := g.Gather()
	if err != nil {
		t.Fatalf("Gather(): %v", err)
	}
	for _, f := range families {
		if f.GetName() == name && len(f.GetMetric()) == 1 {
			return f.GetMetric()[0].GetGauge().GetValue()
		}
	}
	t.Fatalf("%s is not exported", name)
	return 0
}

func histogramCountFrom(t *testing.T, g prometheus.Gatherer, name string) uint64 {
	t.Helper()
	families, err := g.Gather()
	if err != nil {
		t.Fatalf("Gather(): %v", err)
	}
	for _, f := range families {
		if f.GetName() == name && len(f.GetMetric()) == 1 {
			return f.GetMetric()[0].GetHistogram().GetSampleCount()
		}
	}
	t.Fatalf("%s is not exported", name)
	return 0
}
