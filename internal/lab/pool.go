package lab

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrPoolTimeout is returned when a caller gave up before a connection to the
// card processor became free.
var ErrPoolTimeout = errors.New("timed out waiting for a card processor connection")

// Pool is a fixed-size set of connections to the simulated card processor.
//
// It is the model rather than a fault switch, and it is the only saturable
// resource in the lab. Raising the processor's latency holds connections
// longer, which fills the pool, which queues callers, which raises
// checkout-service's client latency — the chain the payment latency runbook
// walks a responder down.
type Pool struct {
	tokens  chan struct{}
	size    int
	metrics *paymentMetrics
}

// NewPool returns a pool of size connections.
func NewPool(size int, m *paymentMetrics) *Pool {
	m.poolSize.Set(float64(size))
	return &Pool{tokens: make(chan struct{}, size), size: size, metrics: m}
}

// Size is the configured number of connections.
func (p *Pool) Size() int { return p.size }

// Acquire takes a connection, waiting until one is free or ctx is done. The
// returned release function must be called, normally by defer, and is safe to
// call more than once.
//
// The wait is observed in both outcomes: a caller that gave up queueing still
// spent that time queueing, and the histogram is the evidence the pool runbook
// tells a responder to read before the in-use ratio.
func (p *Pool) Acquire(ctx context.Context) (release func(), waited time.Duration, err error) {
	start := time.Now()

	select {
	case p.tokens <- struct{}{}:
		// A free connection is taken even if ctx is already on its last
		// millisecond. Answering 503 while the pool sat idle would be a lie
		// about where the problem is.
	default:
		select {
		case p.tokens <- struct{}{}:
		case <-ctx.Done():
			waited = time.Since(start)
			p.metrics.poolWait.Observe(waited.Seconds())
			return nil, waited, ErrPoolTimeout
		}
	}

	waited = time.Since(start)
	p.metrics.poolWait.Observe(waited.Seconds())
	p.metrics.poolInUse.Inc()

	var once sync.Once
	return func() {
		once.Do(func() {
			p.metrics.poolInUse.Dec()
			<-p.tokens
		})
	}, waited, nil
}
