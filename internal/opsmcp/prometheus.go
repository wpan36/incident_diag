package opsmcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// renderedPoints is how many points of a range series reach the model.
//
// A six-hour range at a fifteen-second step is 1440 points per series, which is
// noise in a context window. Evenly spaced samples plus the summary statistics
// below are what a responder actually reads off a graph.
const renderedPoints = 20

// PrometheusQueryArgs is the tool's input. The SDK derives the JSON schema from
// this type, so the field tags are the tool's public contract.
type PrometheusQueryArgs struct {
	Query string `json:"query" jsonschema:"PromQL to evaluate"`
	Start string `json:"start,omitempty" jsonschema:"range query start, RFC 3339 UTC; give with end"`
	End   string `json:"end,omitempty" jsonschema:"range query end, RFC 3339 UTC; give with start"`
	Step  string `json:"step,omitempty" jsonschema:"range query resolution as a Go duration, for example 30s"`
}

func (s *Server) prometheusQuery(ctx context.Context, _ *mcp.CallToolRequest, in PrometheusQueryArgs) (*mcp.CallToolResult, Meta, error) {
	if strings.TrimSpace(in.Query) == "" {
		res, meta := refused("query is required")
		return res, meta, nil
	}

	start, end, ranged, err := parseWindow(in.Start, in.End)
	if err != nil {
		res, meta := refused("%v", err)
		return res, meta, nil
	}

	form := url.Values{"query": {in.Query}}
	endpoint := "/api/v1/query"

	if ranged {
		if span := end.Sub(start); span > s.cfg.MaxRange {
			res, meta := refused("the range is %s; this server allows at most %s", span, s.cfg.MaxRange)
			return res, meta, nil
		}
		step, err := s.resolveStep(in.Step, end.Sub(start))
		if err != nil {
			res, meta := refused("%v", err)
			return res, meta, nil
		}
		endpoint = "/api/v1/query_range"
		form.Set("start", start.Format(timeLayout))
		form.Set("end", end.Format(timeLayout))
		form.Set("step", step.String())
	}

	ctx, cancel := context.WithTimeout(ctx, s.cfg.PrometheusTimeout)
	defer cancel()

	body, kind, err := s.getPrometheus(ctx, endpoint, form)
	if err != nil {
		res, meta := failed(kind, "%v", err)
		return res, meta, nil
	}

	var decoded promResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		res, meta := failed(KindError, "prometheus returned a body this server could not parse: %v", err)
		return res, meta, nil
	}
	// A PromQL syntax error is a 400 with status "error" in the body, and it is
	// something the model can fix, so it is refused rather than reported as a
	// broken dependency.
	if decoded.Status != "success" {
		res, meta := refused("prometheus rejected the query: %s", firstNonEmpty(decoded.Error, decoded.ErrorType, "unknown error"))
		return res, meta, nil
	}
	if n := len(decoded.Data.Result); n > s.cfg.MaxSeries {
		res, meta := refused(
			"the query matched %d series; this server returns at most %d, so narrow it with a label matcher or an aggregation",
			n, s.cfg.MaxSeries)
		return res, meta, nil
	}

	text, points := renderPrometheus(decoded, ranged)
	res, meta := ok(text, Meta{Series: len(decoded.Data.Result), Points: points})
	return res, meta, nil
}

// resolveStep validates an explicit step or derives one from the span.
func (s *Server) resolveStep(raw string, span time.Duration) (time.Duration, error) {
	if raw == "" {
		// Enough points to see a shape, never finer than the minimum.
		step := span / renderedPoints * 5
		if step < s.cfg.MinStep {
			step = s.cfg.MinStep
		}
		return step.Round(time.Second), nil
	}
	step, err := time.ParseDuration(raw)
	switch {
	case err != nil:
		return 0, fmt.Errorf("step must be a duration such as 30s or 1m, got %q", raw)
	case step < s.cfg.MinStep:
		// Refused rather than clamped: a widened step changes the numbers the
		// agent reasons about without telling it.
		return 0, fmt.Errorf("step %s is below this server's minimum of %s", step, s.cfg.MinStep)
	case step > span:
		return 0, fmt.Errorf("step %s is longer than the range itself", step)
	}
	return step, nil
}

// getPrometheus performs the request and classifies a failure.
func (s *Server) getPrometheus(ctx context.Context, path string, form url.Values) ([]byte, string, error) {
	target := strings.TrimSuffix(s.cfg.PrometheusURL, "/") + path + "?" + form.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, KindError, fmt.Errorf("could not build the prometheus request: %w", err)
	}

	res, err := s.http.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, KindTimeout, fmt.Errorf("prometheus did not answer within %s", s.cfg.PrometheusTimeout)
		}
		return nil, KindError, fmt.Errorf("prometheus is unreachable: %w", err)
	}
	defer res.Body.Close()

	// Read the body whatever the status: a 400 carries the PromQL error, which
	// is the most useful thing this tool can tell the model.
	body, readErr := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if readErr != nil {
		return nil, KindError, fmt.Errorf("reading the prometheus response: %w", readErr)
	}
	if res.StatusCode >= 500 {
		return nil, KindError, fmt.Errorf("prometheus answered %s", res.Status)
	}
	return body, "", nil
}

// promResponse is the part of Prometheus's reply this tool reads.
type promResponse struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
	Data      struct {
		ResultType string       `json:"resultType"`
		Result     []promSeries `json:"result"`
	} `json:"data"`
}

type promSeries struct {
	Metric map[string]string `json:"metric"`
	Value  promPoint         `json:"value"`
	Values []promPoint       `json:"values"`
}

// promPoint is [unix_seconds, "value"], which is why it decodes by hand.
type promPoint struct {
	At    time.Time
	Value float64
}

func (p *promPoint) UnmarshalJSON(b []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	if len(raw) != 2 {
		return fmt.Errorf("a sample must be [timestamp, value], got %d elements", len(raw))
	}
	var secs float64
	if err := json.Unmarshal(raw[0], &secs); err != nil {
		return err
	}
	var value string
	if err := json.Unmarshal(raw[1], &value); err != nil {
		return err
	}
	// NaN, +Inf and -Inf are legal Prometheus values and ParseFloat handles
	// them; a value that fails to parse is left as zero rather than failing the
	// whole result, because one bad sample should not lose the other 1439.
	f, _ := strconv.ParseFloat(value, 64)
	p.At = time.Unix(int64(secs), int64((secs-math.Floor(secs))*1e9)).UTC()
	p.Value = f
	return nil
}

// renderPrometheus turns the response into the text the model reads. Raw JSON
// is never passed through: it is mostly punctuation, and the model has a
// bounded context.
func renderPrometheus(r promResponse, ranged bool) (string, int) {
	var b strings.Builder
	total := 0

	if len(r.Data.Result) == 0 {
		return "the query matched no series", 0
	}

	for _, series := range r.Data.Result {
		labels := renderLabels(series.Metric)
		if !ranged {
			fmt.Fprintf(&b, "%s = %s\n", labels, formatValue(series.Value.Value))
			total++
			continue
		}

		if len(series.Values) == 0 {
			fmt.Fprintf(&b, "%s: no points\n", labels)
			continue
		}
		total += len(series.Values)
		st := describe(series.Values)
		fmt.Fprintf(&b, "%s: %d points, min %s at %s, max %s at %s, mean %s, last %s\n",
			labels, len(series.Values),
			formatValue(st.min), st.minAt.Format(timeLayout),
			formatValue(st.max), st.maxAt.Format(timeLayout),
			formatValue(st.mean), formatValue(st.last))
		for _, p := range sample(series.Values, renderedPoints) {
			fmt.Fprintf(&b, "  %s %s\n", p.At.Format(timeLayout), formatValue(p.Value))
		}
	}
	return strings.TrimRight(b.String(), "\n"), total
}

// renderLabels prints a label set in a stable order, so two runs of the same
// query produce the same text and a diff means something changed.
func renderLabels(m map[string]string) string {
	if len(m) == 0 {
		return "{}"
	}
	name := m["__name__"]
	keys := make([]string, 0, len(m))
	for k := range m {
		if k != "__name__" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%q", k, m[k]))
	}
	return name + "{" + strings.Join(parts, ",") + "}"
}

type stats struct {
	min, max, mean, last float64
	minAt, maxAt         time.Time
}

func describe(points []promPoint) stats {
	st := stats{min: points[0].Value, max: points[0].Value, minAt: points[0].At, maxAt: points[0].At}
	sum := 0.0
	for _, p := range points {
		if p.Value < st.min {
			st.min, st.minAt = p.Value, p.At
		}
		if p.Value > st.max {
			st.max, st.maxAt = p.Value, p.At
		}
		sum += p.Value
	}
	st.mean = sum / float64(len(points))
	st.last = points[len(points)-1].Value
	return st
}

// sample takes n evenly spaced points, always including the first and the last.
func sample(points []promPoint, n int) []promPoint {
	if len(points) <= n {
		return points
	}
	out := make([]promPoint, 0, n)
	for i := range n {
		idx := i * (len(points) - 1) / (n - 1)
		out = append(out, points[idx])
	}
	return out
}

// formatValue prints a number the way a responder reads one: enough precision
// to be useful, not enough to be noise.
func formatValue(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "+Inf"
	case math.IsInf(f, -1):
		return "-Inf"
	}
	return strconv.FormatFloat(f, 'g', 6, 64)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
