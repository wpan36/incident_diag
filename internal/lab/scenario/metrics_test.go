package scenario

import (
	"strings"
	"testing"
	"time"

	"github.com/wpan36/incident_diag/internal/lab"
)

// recorded is a trimmed but otherwise real /metrics body from payment-service.
const recorded = `# HELP payment_pool_size Configured size of the card processor connection pool.
# TYPE payment_pool_size gauge
payment_pool_size 20
# HELP payment_pool_in_use Card processor connections currently held.
# TYPE payment_pool_in_use gauge
payment_pool_in_use 18
# HELP payment_pool_wait_seconds Time spent waiting for a card processor connection.
# TYPE payment_pool_wait_seconds histogram
payment_pool_wait_seconds_bucket{le="0.5"} 100
payment_pool_wait_seconds_bucket{le="+Inf"} 200
payment_pool_wait_seconds_sum 40
payment_pool_wait_seconds_count 200
# HELP http_request_duration_seconds Duration of HTTP requests handled by this service.
# TYPE http_request_duration_seconds histogram
http_request_duration_seconds_bucket{method="POST",route="/charge",status="200",le="+Inf"} 180
http_request_duration_seconds_sum{method="POST",route="/charge",status="200"} 450
http_request_duration_seconds_count{method="POST",route="/charge",status="200"} 180
http_request_duration_seconds_bucket{method="POST",route="/charge",status="503",le="+Inf"} 20
http_request_duration_seconds_sum{method="POST",route="/charge",status="503"} 40
http_request_duration_seconds_count{method="POST",route="/charge",status="503"} 20
`

func TestParseSnapshot(t *testing.T) {
	s, err := parseSnapshot(strings.NewReader(recorded))
	if err != nil {
		t.Fatalf("parseSnapshot(): %v", err)
	}

	if got := s.gauge(lab.MetricPoolInUse); got != 18 {
		t.Errorf("%s = %v, want 18", lab.MetricPoolInUse, got)
	}
	if got := s.gauge(lab.MetricPoolSize); got != 20 {
		t.Errorf("%s = %v, want 20", lab.MetricPoolSize, got)
	}
	if got := s.histograms[lab.MetricPoolWait]; got.count != 200 || got.sum != 40 {
		t.Errorf("%s = %+v, want 200 observations summing to 40", lab.MetricPoolWait, got)
	}
	// The request histogram is summed per route and status, because the 5xx
	// rate the error rate runbook is about is a count of statuses.
	if got := s.requests["/charge 503"]; got != 20 {
		t.Errorf("/charge 503 = %d, want 20", got)
	}
	if got := s.requests["/charge 200"]; got != 180 {
		t.Errorf("/charge 200 = %d, want 180", got)
	}
}

func TestParseSnapshotRejectsGarbage(t *testing.T) {
	if _, err := parseSnapshot(strings.NewReader("this is not the text format {\n")); err == nil {
		t.Error("parseSnapshot() accepted a body that is not the Prometheus text format")
	}
}

func TestDeltasBetweenSnapshots(t *testing.T) {
	before := snapshot{
		histograms: map[string]histogram{lab.MetricPoolWait: {sum: 1, count: 10}},
		counters:   map[string]float64{lab.MetricClientTimeouts: 5},
		requests:   map[string]uint64{"payment-service /charge 503": 2},
	}
	after := snapshot{
		histograms: map[string]histogram{lab.MetricPoolWait: {sum: 21, count: 20}},
		counters:   map[string]float64{lab.MetricClientTimeouts: 45},
		requests:   map[string]uint64{"payment-service /charge 503": 42, "payment-service /charge 200": 100},
	}

	// The mean is over the window rather than over all time: a phase's numbers
	// have to be readable against that phase, not against everything since the
	// process started.
	count, mean := histogramDelta(before, after, lab.MetricPoolWait)
	if count != 10 || mean != 2*time.Second {
		t.Errorf("histogramDelta() = %d observations, mean %v; want 10 and 2s", count, mean)
	}
	if got := counterDelta(before, after, lab.MetricClientTimeouts); got != 40 {
		t.Errorf("counterDelta() = %v, want 40", got)
	}
	if got := requestsByStatus(before, after, "payment-service", "5"); got != 40 {
		t.Errorf("requestsByStatus(5xx) = %d, want 40", got)
	}
	// A route contains a space, so the status has to be read as the last field
	// rather than the second.
	if got := requestsByStatus(before, after, "payment-service", "2"); got != 100 {
		t.Errorf("requestsByStatus(2xx) = %d, want 100", got)
	}
	if got := requestsByStatus(before, after, "checkout-service", "5"); got != 0 {
		t.Errorf("requestsByStatus() = %d for a service with no samples, want 0", got)
	}
}

func TestMergeKeepsBothServicesRequestDurations(t *testing.T) {
	checkout := snapshot{
		histograms: map[string]histogram{lab.MetricRequestDuration: {sum: 1, count: 1}},
		gauges:     map[string]float64{}, counters: map[string]float64{},
		requests: map[string]uint64{"/orders 201": 5},
	}
	payment := snapshot{
		histograms: map[string]histogram{lab.MetricRequestDuration: {sum: 9, count: 3}},
		gauges:     map[string]float64{lab.MetricPoolSize: 20}, counters: map[string]float64{},
		requests: map[string]uint64{"/charge 200": 5},
	}

	got := merge(checkout, payment)

	// Both services export http_request_duration_seconds. Summing them would
	// produce a mean nobody measured.
	if h := got.histograms[lab.MetricRequestDuration]; h.count != 1 {
		t.Errorf("merged request duration = %+v, want checkout's", h)
	}
	if h := got.histograms["payment:"+lab.MetricRequestDuration]; h.count != 3 {
		t.Errorf("payment's request duration = %+v, want it kept separately", h)
	}
	if got.requests["checkout-service /orders 201"] != 5 || got.requests["payment-service /charge 200"] != 5 {
		t.Errorf("merged requests = %v, want both services keyed by name", got.requests)
	}
	if got.gauge(lab.MetricPoolSize) != 20 {
		t.Error("the pool gauge was lost in the merge")
	}
}
