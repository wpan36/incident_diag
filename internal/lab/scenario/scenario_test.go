package scenario

import (
	"strings"
	"testing"
	"time"
)

func TestFindAndList(t *testing.T) {
	for _, name := range Names() {
		if _, err := Find(name); err != nil {
			t.Errorf("Find(%q): %v", name, err)
		}
	}
	if _, err := Find("nope"); err == nil {
		t.Error("Find() accepted an unknown scenario")
	} else if !strings.Contains(err.Error(), "payment-latency") {
		t.Errorf("error = %v, want it to name the scenarios that do exist", err)
	}

	list := List()
	for _, name := range Names() {
		if !strings.Contains(list, name) {
			t.Errorf("List() does not mention %s", name)
		}
	}
}

func TestEveryScenarioNamesItsRunbook(t *testing.T) {
	// The corpus is what makes a scenario meaningful: a fault nothing in
	// testdata/knowledge diagnoses is a failure the agent cannot be expected to
	// explain, and M32 grades against these.
	for _, s := range All() {
		if s.Runbook == "" || s.Expect == "" {
			t.Errorf("scenario %s is missing its expected shape or its runbook", s.Name)
		}
	}
}

func TestBaselineInjectsNothing(t *testing.T) {
	s, err := Find("baseline")
	if err != nil {
		t.Fatalf("Find(): %v", err)
	}
	// The negative case. M32 needs scenarios where the right answer is that
	// nothing is wrong, or a diagnosis rate means nothing.
	if s.Fault != nil {
		t.Error("the baseline scenario injects a fault")
	}
}

func TestFaultPayloads(t *testing.T) {
	params := Params{DelayMS: 1000, JitterMS: 100, Status: 500, Ratio: 0.5, Workers: 3}
	for _, tc := range []struct {
		scenario string
		target   string
		kind     string
	}{
		{"payment-latency", targetPayment, "latency"},
		{"checkout-cpu", targetCheckout, "cpu"},
		{"payment-errors", targetPayment, "error"},
	} {
		t.Run(tc.scenario, func(t *testing.T) {
			s, err := Find(tc.scenario)
			if err != nil {
				t.Fatalf("Find(): %v", err)
			}
			call := s.Fault(params)
			if call.target != tc.target {
				t.Errorf("target = %q, want %q", call.target, tc.target)
			}
			if call.body["kind"] != tc.kind {
				t.Errorf("kind = %v, want %q", call.body["kind"], tc.kind)
			}
			if call.body["enabled"] != true {
				t.Errorf("enabled = %v, want true", call.body["enabled"])
			}
		})
	}
}

func TestPercentile(t *testing.T) {
	ds := []time.Duration{
		10 * time.Millisecond, 20 * time.Millisecond, 30 * time.Millisecond,
		40 * time.Millisecond, 50 * time.Millisecond, 60 * time.Millisecond,
		70 * time.Millisecond, 80 * time.Millisecond, 90 * time.Millisecond,
		100 * time.Millisecond,
	}
	for _, tc := range []struct {
		p    float64
		want time.Duration
	}{
		{50, 50 * time.Millisecond},
		{90, 90 * time.Millisecond},
		{95, 100 * time.Millisecond},
		{99, 100 * time.Millisecond},
		{100, 100 * time.Millisecond},
	} {
		if got := percentile(ds, tc.p); got != tc.want {
			t.Errorf("percentile(p%v) = %v, want %v", tc.p, got, tc.want)
		}
	}

	// An empty phase is a real case — every request failed to connect — and it
	// must not panic on the way to the report.
	if got := percentile(nil, 99); got != 0 {
		t.Errorf("percentile of nothing = %v, want 0", got)
	}
}

func TestStatusSummaryIsOrdered(t *testing.T) {
	r := &routeStats{statuses: map[int]int{504: 12, 201: 210, 0: 3}}
	// Ordered so two phases line up when read one under the other, and 0
	// rendered as "none" because no service sent it.
	if got, want := statusSummary(r), "none×3 201×210 504×12"; got != want {
		t.Errorf("statusSummary() = %q, want %q", got, want)
	}
}

func TestMeanMax(t *testing.T) {
	mean, max := meanMax([]float64{2, 4, 18})
	if mean != 8 || max != 18 {
		t.Errorf("meanMax() = %v/%v, want 8/18", mean, max)
	}
	if mean, max := meanMax(nil); mean != 0 || max != 0 {
		t.Errorf("meanMax(nil) = %v/%v, want 0/0", mean, max)
	}
}
