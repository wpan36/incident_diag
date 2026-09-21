package opsmcp

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

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

	derived, err := s.resolveStep("", span)
	if err != nil || derived < s.cfg.MinStep {
		t.Errorf("derived step = %v, err = %v", derived, err)
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

	text, points := renderPrometheus(r, false)
	if points != 1 {
		t.Errorf("points = %d", points)
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

	text, points := renderPrometheus(r, true)
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
	text, points := renderPrometheus(r, false)
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
	mustJSON(t, `{"status":"success","data":{"result":[
      {"metric":{"__name__":"q"},"value":[1758448800,"NaN"]}]}}`, &r)
	if text, _ := renderPrometheus(r, false); !strings.Contains(text, "NaN") {
		t.Errorf("render = %q", text)
	}
}

func mustJSON(t *testing.T, body string, into any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), into); err != nil {
		t.Fatalf("decoding the fixture: %v", err)
	}
}
