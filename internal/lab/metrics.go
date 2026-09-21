package lab

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Metric names. They are constants because the knowledge corpus in
// testdata/knowledge names them: a runbook tells a responder to check
// payment_pool_in_use, and renaming it here turns that diagnostic step into a
// query that returns nothing. registeredNames in metrics_test.go asserts each
// one is actually exported.
const (
	MetricRequestDuration = "http_request_duration_seconds"

	MetricPoolSize         = "payment_pool_size"
	MetricPoolInUse        = "payment_pool_in_use"
	MetricPoolWait         = "payment_pool_wait_seconds"
	MetricProcessorLatency = "payment_processor_latency_seconds"

	MetricClientLatency  = "checkout_payment_client_latency_seconds"
	MetricClientTimeouts = "checkout_payment_client_timeouts_total"
)

// newRegistry returns a registry holding the Go runtime and process
// collectors.
//
// A registry per service rather than the global one: it is what lets a test
// build a whole service and read its metrics without the previous test's
// numbers still being in there.
func newRegistry() *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return reg
}

// requestMetrics is the one metric both lab services export.
//
// The default buckets reach 10s, past both latency alert thresholds the corpus
// quotes (payment 2s, checkout 3s), so histogram_quantile has resolution where
// the runbooks look.
type requestMetrics struct {
	duration *prometheus.HistogramVec
}

func newRequestMetrics(reg prometheus.Registerer) *requestMetrics {
	m := &requestMetrics{
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    MetricRequestDuration,
			Help:    "Duration of HTTP requests handled by this service.",
			Buckets: prometheus.DefBuckets,
		}, []string{"method", "route", "status"}),
	}
	reg.MustRegister(m.duration)
	return m
}

func (m *requestMetrics) observe(method, route, status string, d time.Duration) {
	m.duration.WithLabelValues(method, route, status).Observe(d.Seconds())
}

// paymentMetrics are payment-service's own metrics: the pool, and the
// simulated card processor behind it.
type paymentMetrics struct {
	poolSize         prometheus.Gauge
	poolInUse        prometheus.Gauge
	poolWait         prometheus.Histogram
	processorLatency prometheus.Histogram
}

func newPaymentMetrics(reg prometheus.Registerer) *paymentMetrics {
	m := &paymentMetrics{
		poolSize: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: MetricPoolSize,
			Help: "Configured size of the card processor connection pool.",
		}),
		poolInUse: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: MetricPoolInUse,
			Help: "Card processor connections currently held.",
		}),
		// The pool runbook says to read the wait time before the in-use ratio:
		// a pool at full utilization with a flat wait time is exactly the right
		// size. These buckets therefore have to resolve short waits as well as
		// the seconds-long ones a saturated pool produces.
		poolWait: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    MetricPoolWait,
			Help:    "Time spent waiting for a card processor connection.",
			Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		}),
		processorLatency: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    MetricProcessorLatency,
			Help:    "Time spent in the simulated card processor, connection wait excluded.",
			Buckets: prometheus.DefBuckets,
		}),
	}
	reg.MustRegister(m.poolSize, m.poolInUse, m.poolWait, m.processorLatency)
	return m
}

// checkoutMetrics are checkout-service's view of its one synchronous
// dependency.
type checkoutMetrics struct {
	clientLatency  prometheus.Histogram
	clientTimeouts prometheus.Counter
}

func newCheckoutMetrics(reg prometheus.Registerer) *checkoutMetrics {
	m := &checkoutMetrics{
		clientLatency: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    MetricClientLatency,
			Help:    "Latency of calls to payment-service, as checkout-service sees it.",
			Buckets: prometheus.DefBuckets,
		}),
		// The corpus's correctness metric: each increment is an authorization
		// that may have succeeded on the other side of a call this service
		// abandoned.
		clientTimeouts: prometheus.NewCounter(prometheus.CounterOpts{
			Name: MetricClientTimeouts,
			Help: "Calls to payment-service abandoned at the client timeout.",
		}),
	}
	reg.MustRegister(m.clientLatency, m.clientTimeouts)
	return m
}
