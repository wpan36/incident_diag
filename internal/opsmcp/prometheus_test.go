package opsmcp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wpan36/incident_diag/internal/config"
)

func testServer() *Server {
	return New(config.OpsMCP{
		MaxRange:  6 * time.Hour,
		MinStep:   15 * time.Second,
		MaxSeries: 50,
	}, nil)
}

func TestParseWindow(t *testing.T) {
	_, _, ranged, err := parseWindow("", "")
	if err != nil || ranged {
		t.Errorf("no window: ranged=%v err=%v", ranged, err)
	}

	// Exactly one endpoint is refused, not guessed: "from then until now" and
	// "from the beginning until then" are both plausible readings.
	for _, c := range [][2]string{{"2026-09-21T10:00:00Z", ""}, {"", "2026-09-21T10:00:00Z"}} {
		if _, _, _, err := parseWindow(c[0], c[1]); err == nil {
			t.Errorf("parseWindow(%q, %q) was accepted", c[0], c[1])
		}
	}

	if _, _, _, err := parseWindow("2026-09-21T10:00:00Z", "2026-09-21T09:00:00Z"); err == nil {
		t.Error("an end before the start was accepted")
	}
	// Relative times are deliberately not accepted.
	if _, _, _, err := parseWindow("-5m", "now"); err == nil {
		t.Error("a relative time was accepted")
	}

	start, end, ranged, err := parseWindow("2026-09-21T10:00:00+02:00", "2026-09-21T11:00:00+02:00")
	if err != nil || !ranged {
		t.Fatalf("ranged=%v err=%v", ranged, err)
	}
	if start.Location() != time.UTC || end.Location() != time.UTC {
		t.Error("timestamps were not normalized to UTC")
	}
}

func TestResolveStep(t *testing.T) {
	s := testServer()
	span := time.Hour

	// The spec fixes the derived step at the span over stepDivisor. Asserting
	// only that it clears the minimum would accept a step a hundred times too
	// coarse, which is a graph with four points on it.
	derived, err := s.resolveStep("", span)
	if err != nil || derived != span/stepDivisor {
		t.Errorf("derived step = %v, want %v (err %v)", derived, span/stepDivisor, err)
	}

	if _, err := s.resolveStep("1s", span); err == nil {
		// Refused rather than clamped: a widened step changes the numbers the
		// agent reasons about without telling it.
		t.Error("a step below the minimum was accepted")
	}
	if _, err := s.resolveStep("2h", span); err == nil {
		t.Error("a step longer than the range was accepted")
	}
	if _, err := s.resolveStep("often", span); err == nil {
		t.Error("a malformed step was accepted")
	}
	if got, err := s.resolveStep("30s", span); err != nil || got != 30*time.Second {
		t.Errorf("resolveStep(30s) = %v, %v", got, err)
	}
}

func TestResolveStepNeverGoesBelowTheMinimumOnAShortRange(t *testing.T) {
	s := testServer()
	got, err := s.resolveStep("", 10*time.Second)
	if err != nil {
		t.Fatalf("resolveStep: %v", err)
	}
	if got < s.cfg.MinStep {
		t.Errorf("derived step %v is below the minimum %v", got, s.cfg.MinStep)
	}
}

func TestRenderPrometheusInstant(t *testing.T) {
	var r promResponse
	mustJSON(t, `{"status":"success","data":{"resultType":"vector","result":[
      {"metric":{"__name__":"up","job":"payment-service","instance":"x:8080"},"value":[1758448800,"1"]}]}}`, &r)

	text, series, points := renderPrometheus(decode(t, r))
	if points != 1 || series != 1 {
		t.Errorf("series = %d, points = %d", series, points)
	}
	// Labels in a stable order, so two runs of the same query produce the same
	// text and a diff means something changed.
	if !strings.Contains(text, `up{instance="x:8080",job="payment-service"} = 1`) {
		t.Errorf("render = %q", text)
	}
}

func TestRenderPrometheusRangeSummarizes(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"__name__":"q"},"values":[`)
	for i := range 100 {
		if i > 0 {
			b.WriteString(",")
		}
		// A single spike, so min, max and their timestamps are checkable.
		v := 1
		if i == 50 {
			v = 99
		}
		b.WriteString(`[` + strconv.Itoa(1758448800+i*15) + `,"` + strconv.Itoa(v) + `"]`)
	}
	b.WriteString(`]}]}}`)

	var r promResponse
	mustJSON(t, b.String(), &r)

	text, _, points := renderPrometheus(decode(t, r))
	if points != 100 {
		t.Errorf("points = %d, want 100", points)
	}
	for _, want := range []string{"100 points", "min 1 at", "max 99 at", "mean ", "last 1"} {
		if !strings.Contains(text, want) {
			t.Errorf("render is missing %q:\n%s", want, text)
		}
	}
	// Raw JSON is never passed through, and 100 points would be noise in a
	// context window.
	rendered := strings.Count(text, "\n  ")
	if rendered != renderedPoints {
		t.Errorf("rendered %d sample lines, want %d", rendered, renderedPoints)
	}
}

func TestRenderPrometheusNoSeries(t *testing.T) {
	var r promResponse
	mustJSON(t, `{"status":"success","data":{"resultType":"vector","result":[]}}`, &r)
	text, _, points := renderPrometheus(decode(t, r))
	if points != 0 || !strings.Contains(text, "matched no series") {
		t.Errorf("render = %q", text)
	}
}

func TestSampleKeepsTheEnds(t *testing.T) {
	points := make([]promPoint, 100)
	for i := range points {
		points[i] = promPoint{Value: float64(i)}
	}
	got := sample(points, 20)
	if len(got) != 20 || got[0].Value != 0 || got[19].Value != 99 {
		t.Errorf("sample kept %d points, first %v last %v", len(got), got[0].Value, got[19].Value)
	}
	// Fewer points than asked for come back whole rather than padded.
	if got := sample(points[:5], 20); len(got) != 5 {
		t.Errorf("sample of a short series returned %d", len(got))
	}
}

func TestFormatValueHandlesSpecials(t *testing.T) {
	var r promResponse
	// NaN is a legal Prometheus value; it must not become 0 or break the render.
	mustJSON(t, `{"status":"success","data":{"resultType":"vector","result":[
      {"metric":{"__name__":"q"},"value":[1758448800,"NaN"]}]}}`, &r)
	if text, _, _ := renderPrometheus(decode(t, r)); !strings.Contains(text, "NaN") {
		t.Errorf("render = %q", text)
	}
}

// decode runs the resultType-driven decoding the handler runs, so the tests
// exercise the same path rather than constructing promResult by hand.
func decode(t *testing.T, r promResponse) promResult {
	t.Helper()
	out, err := decodeResult(r)
	if err != nil {
		t.Fatalf("decodeResult: %v", err)
	}
	return out
}

func mustJSON(t *testing.T, body string, into any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), into); err != nil {
		t.Fatalf("decoding the fixture: %v", err)
	}
}

// fakePrometheus serves a canned body and records the query string it was
// asked for.
//
// This round trip is what the package was missing: every test above stops at
// the renderer, so nothing exercised the status handling, the result-type
// decoding or the step that actually goes on the wire.
func fakePrometheus(t *testing.T, status int, body string) (*Server, *url.Values) {
	t.Helper()
	var asked url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Query()
		asked.Set("path", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)

	s := testServer()
	s.cfg.PrometheusURL = srv.URL
	s.cfg.PrometheusTimeout = 5 * time.Second
	return s, &asked
}

func query(t *testing.T, s *Server, in PrometheusQueryArgs) (*mcp.CallToolResult, Meta, string) {
	t.Helper()
	res, meta, err := s.prometheusQuery(t.Context(), nil, in)
	if err != nil {
		t.Fatalf("prometheusQuery: %v", err)
	}
	return res, meta, res.Content[0].(*mcp.TextContent).Text
}

func TestPrometheusQueryInstantRoundTrip(t *testing.T) {
	s, asked := fakePrometheus(t, 200, `{"status":"success","data":{"resultType":"vector","result":[
	  {"metric":{"__name__":"up","job":"payment-service"},"value":[1758448800,"1"]}]}}`)

	res, meta, text := query(t, s, PrometheusQueryArgs{Query: "up"})
	if res.IsError || meta.Kind != KindOK || meta.Refused {
		t.Fatalf("kind=%s refused=%v text=%q", meta.Kind, meta.Refused, text)
	}
	if asked.Get("path") != "/api/v1/query" || asked.Get("query") != "up" {
		t.Errorf("asked for %v", *asked)
	}
	if !strings.Contains(text, `up{job="payment-service"} = 1`) {
		t.Errorf("text = %q", text)
	}
}

func TestPrometheusQueryRangeSendsTheDerivedStep(t *testing.T) {
	s, asked := fakePrometheus(t, 200, `{"status":"success","data":{"resultType":"matrix","result":[
	  {"metric":{"__name__":"up"},"values":[[1758448800,"1"],[1758448836,"0"]]}]}}`)

	// One hour over stepDivisor is 36s. A step derived from the wrong divisor
	// would be coarse enough to delete the incident the agent is looking for.
	_, meta, text := query(t, s, PrometheusQueryArgs{
		Query: "up", Start: "2026-09-21T10:00:00Z", End: "2026-09-21T11:00:00Z"})
	if meta.Refused {
		t.Fatalf("refused: %q", text)
	}
	if asked.Get("path") != "/api/v1/query_range" || asked.Get("step") != "36s" {
		t.Errorf("asked for %v", *asked)
	}
	if !strings.Contains(text, "2 points") {
		t.Errorf("text = %q", text)
	}
}

func TestPrometheusQueryRendersByResultTypeNotByRequestShape(t *testing.T) {
	// "up[5m]" is an instant query that returns a matrix. Rendering it as a
	// vector reads the absent single value and prints 0 — a number that is not
	// in the data, from which the model would conclude the service is down.
	s, _ := fakePrometheus(t, 200, `{"status":"success","data":{"resultType":"matrix","result":[
	  {"metric":{"__name__":"up","job":"payment-service"},"values":[[1758448800,"1"],[1758448815,"1"]]}]}}`)

	_, meta, text := query(t, s, PrometheusQueryArgs{Query: "up[5m]"})
	if strings.Contains(text, "= 0") {
		t.Errorf("a matrix was rendered as a vector: %q", text)
	}
	if !strings.Contains(text, "2 points") || meta.Points != 2 {
		t.Errorf("text = %q points = %d", text, meta.Points)
	}
}

func TestPrometheusQueryRendersScalarAndString(t *testing.T) {
	s, _ := fakePrometheus(t, 200, `{"status":"success","data":{"resultType":"scalar","result":[1758448800,"2"]}}`)
	res, meta, text := query(t, s, PrometheusQueryArgs{Query: "scalar(up)"})
	if res.IsError || !strings.Contains(text, "= 2") {
		t.Errorf("scalar: isError=%v text=%q", res.IsError, text)
	}
	if meta.Series != 1 || meta.Points != 1 {
		t.Errorf("scalar: series=%d points=%d", meta.Series, meta.Points)
	}

	s, _ = fakePrometheus(t, 200, `{"status":"success","data":{"resultType":"string","result":[1758448800,"hello"]}}`)
	res, _, text = query(t, s, PrometheusQueryArgs{Query: `"hello"`})
	if res.IsError || !strings.Contains(text, `"hello"`) {
		t.Errorf("string: isError=%v text=%q", res.IsError, text)
	}
}

func TestPrometheusQueryRefusesAPromQLErrorButNotAMisconfiguredEndpoint(t *testing.T) {
	// A 400 is the model's problem and is refused, so it can fix the query.
	s, _ := fakePrometheus(t, 400, `{"status":"error","errorType":"bad_data","error":"1:1: parse error"}`)
	res, meta, text := query(t, s, PrometheusQueryArgs{Query: "up{"})
	if res.IsError || !meta.Refused || !strings.Contains(text, "parse error") {
		t.Errorf("400: isError=%v refused=%v text=%q", res.IsError, meta.Refused, text)
	}

	// A 404 or a 401 is not a query API response at all. Reporting it as a
	// rejected query would have the model rewrite a good query until its
	// budget ran out.
	for _, status := range []int{401, 404} {
		s, _ := fakePrometheus(t, status, `{}`)
		res, meta, text := query(t, s, PrometheusQueryArgs{Query: "up"})
		if !res.IsError || meta.Kind != KindError || meta.Refused {
			t.Errorf("%d: isError=%v kind=%s refused=%v text=%q", status, res.IsError, meta.Kind, meta.Refused, text)
		}
	}
}
