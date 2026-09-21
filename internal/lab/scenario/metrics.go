package scenario

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"

	"github.com/wpan36/incident_diag/internal/lab"
)

// snapshot is one service's /metrics at one moment.
//
// Scraping the services rather than Prometheus is deliberate: it means a
// scenario verifies itself with nothing else running, so a disagreement between
// this report and a dashboard later is a question about the dashboard rather
// than about whether the incident happened.
type snapshot struct {
	at         time.Time
	gauges     map[string]float64
	counters   map[string]float64
	histograms map[string]histogram
	// requests is http_request_duration_seconds' count, keyed by
	// "route status".
	requests map[string]uint64
}

type histogram struct {
	sum   float64
	count uint64
}

func (s snapshot) gauge(name string) float64   { return s.gauges[name] }
func (s snapshot) counter(name string) float64 { return s.counters[name] }

// histogramDelta returns the observations and the mean latency added between
// two snapshots.
func histogramDelta(before, after snapshot, name string) (count uint64, mean time.Duration) {
	b, a := before.histograms[name], after.histograms[name]
	count = a.count - b.count
	if count == 0 {
		return 0, 0
	}
	return count, time.Duration((a.sum - b.sum) / float64(count) * float64(time.Second))
}

// counterDelta returns how much a counter moved between two snapshots.
func counterDelta(before, after snapshot, name string) float64 {
	return after.counter(name) - before.counter(name)
}

// requestsByStatus returns how many requests one service answered with a
// status starting with statusPrefix, between two snapshots and across every
// route. A prefix of "5" is the 5xx rate the error rate runbook is about.
//
// A merged snapshot's keys are "<service> <route> <status>", and the status is
// the last field because a route contains spaces of its own.
func requestsByStatus(before, after snapshot, service, statusPrefix string) uint64 {
	var total uint64
	for key, count := range after.requests {
		if !strings.HasPrefix(key, service+" ") {
			continue
		}
		status := key[strings.LastIndex(key, " ")+1:]
		if !strings.HasPrefix(status, statusPrefix) {
			continue
		}
		total += count - before.requests[key]
	}
	return total
}

// scrape reads one service's /metrics.
func scrape(ctx context.Context, client *http.Client, baseURL string) (snapshot, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/metrics", nil)
	if err != nil {
		return snapshot{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return snapshot{}, fmt.Errorf("scraping %s: %w", baseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return snapshot{}, fmt.Errorf("scraping %s: %s", baseURL, resp.Status)
	}
	return parseSnapshot(resp.Body)
}

// parseSnapshot reads the Prometheus text format. The parser is the one the
// client library already ships, rather than a second implementation of a format
// that has edge cases.
func parseSnapshot(r io.Reader) (snapshot, error) {
	// The validation scheme is passed explicitly. The zero TextParser reads a
	// package-level default that the library leaves unset and panics on, and a
	// lab service's metric names are plain ASCII either way.
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(r)
	if err != nil {
		return snapshot{}, fmt.Errorf("parsing the metrics response: %w", err)
	}

	s := snapshot{
		at:         time.Now(),
		gauges:     map[string]float64{},
		counters:   map[string]float64{},
		histograms: map[string]histogram{},
		requests:   map[string]uint64{},
	}
	for name, f := range families {
		for _, m := range f.GetMetric() {
			switch f.GetType() {
			case dto.MetricType_GAUGE:
				s.gauges[name] += m.GetGauge().GetValue()
			case dto.MetricType_COUNTER:
				s.counters[name] += m.GetCounter().GetValue()
			case dto.MetricType_HISTOGRAM:
				h := s.histograms[name]
				h.sum += m.GetHistogram().GetSampleSum()
				h.count += m.GetHistogram().GetSampleCount()
				s.histograms[name] = h
				if name == lab.MetricRequestDuration {
					s.requests[requestKey(m)] += m.GetHistogram().GetSampleCount()
				}
			}
		}
	}
	return s, nil
}

func requestKey(m *dto.Metric) string {
	var route, status string
	for _, l := range m.GetLabel() {
		switch l.GetName() {
		case "route":
			route = l.GetValue()
		case "status":
			status = l.GetValue()
		}
	}
	return route + " " + status
}
