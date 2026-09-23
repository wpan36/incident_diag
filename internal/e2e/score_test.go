package e2e

import (
	"strings"
	"testing"
	"time"
)

func TestMark(t *testing.T) {
	latency := scenarioNamed(t, "payment-latency").Expect
	cpu := scenarioNamed(t, "payment-cpu").Expect
	baseline := scenarioNamed(t, "baseline").Expect

	cases := []struct {
		name    string
		expect  Expectation
		service string
		cause   string
		want    bool
		why     string
	}{
		{
			name: "the processor diagnosis is correct", expect: latency,
			service: "payment-service",
			cause:   "The card processor behind payment-service is slow, which fills its connection pool.",
			want:    true,
		},
		{
			name: "the pool diagnosis is correct too", expect: latency,
			service: "payment-service",
			cause:   "payment-service's connection pool is saturated waiting on a slow downstream call.",
			want:    true,
		},
		{
			name: "a qualified service name still names the service", expect: latency,
			service: "payment-service (root cause upstream, in the card processor it calls)",
			cause:   "The card processor is slow.",
			want:    true,
		},
		{
			name: "the wrong service is wrong", expect: latency,
			service: "checkout-service",
			cause:   "checkout's client timeout is too short for a healthy payment-service.",
			want:    false, why: "want payment-service",
		},
		{
			name: "the right service with the other failure mode is wrong", expect: latency,
			service: "payment-service",
			cause:   "payment-service is CPU saturated, so every request is slow.",
			want:    false, why: "cpu satur",
		},
		{
			name: "CPU is correct for the CPU scenario", expect: cpu,
			service: "payment-service",
			cause:   "payment-service is CPU saturated; the card processor itself is fine.",
			want:    true,
		},
		{
			name: "a diagnosis naming no mechanism is wrong", expect: latency,
			service: "payment-service",
			cause:   "payment-service is slow.",
			want:    false, why: "names none of",
		},
		{
			name: "the healthy case is correct when it says so", expect: baseline,
			service: "",
			cause:   "Nothing is wrong: every latency and error rate is within budget.",
			want:    true,
		},
		{
			name: "the healthy case is correct even if it names a service", expect: baseline,
			service: "payment-service",
			cause:   "No root cause: payment-service and checkout-service are both operating normally.",
			want:    true,
		},
		{
			name: "an invented root cause fails the healthy case", expect: baseline,
			service: "payment-service",
			cause:   "The connection pool exhaustion is degrading checkout.",
			want:    false, why: "invented",
		},
		{
			name: "a hedged healthy answer that names nothing is wrong", expect: baseline,
			service: "checkout-service",
			cause:   "It is not clear what is happening; more data is needed.",
			want:    false, why: "did not conclude",
		},
		{
			name: "a run with no diagnosis is wrong", expect: latency,
			service: "", cause: "  ",
			want: false, why: "no diagnosis",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.expect.Mark(Outcome{AffectedService: tc.service, RootCause: tc.cause})
			if got.Correct != tc.want {
				t.Fatalf("Correct = %v, want %v (why: %s)", got.Correct, tc.want, got.Why)
			}
			if tc.why != "" && !strings.Contains(got.Why, tc.why) {
				t.Errorf("Why = %q, want it to mention %q", got.Why, tc.why)
			}
			if got.Correct && got.Why != "" {
				t.Errorf("a correct run carried a reason: %q", got.Why)
			}
		})
	}
}

// The set has to keep the shape the evaluation claims, or the accuracy number
// means something other than what the report says it does.
func TestEvalScenariosAreBalanced(t *testing.T) {
	scenarios := EvalScenarios()
	if len(scenarios) < 8 {
		t.Fatalf("%d scenarios, want at least 8", len(scenarios))
	}

	byService := map[string]int{}
	healthy := 0
	names := map[string]bool{}
	for _, s := range scenarios {
		if names[s.Name] {
			t.Errorf("two scenarios are called %s", s.Name)
		}
		names[s.Name] = true

		if s.Expect.Healthy {
			healthy++
			if s.Fault != "" && s.Fault != "payment-latency-mild" {
				t.Errorf("%s is a negative case with fault %q", s.Name, s.Fault)
			}
			continue
		}
		if s.Expect.Service == "" {
			t.Errorf("%s expects no service and is not a negative case", s.Name)
		}
		if len(s.Expect.Any) == 0 {
			t.Errorf("%s would be marked on the service alone", s.Name)
		}
		byService[s.Expect.Service]++
	}

	if healthy < 2 {
		t.Errorf("%d negative cases, want at least 2: an evaluation with no way to be wrong "+
			"by inventing a root cause measures only enthusiasm", healthy)
	}
	// If every fault pointed at one service, naming it would be the whole
	// answer and the accuracy would be a coin the agent cannot lose.
	if len(byService) < 2 {
		t.Errorf("every fault points at one service: %v", byService)
	}
}

func TestReportRendersEveryRun(t *testing.T) {
	s := scenarioNamed(t, "payment-latency")
	results := []Result{
		{Scenario: s, Attempt: 1, Score: Score{Correct: true, ServiceOK: true, CauseOK: true},
			Outcome: Outcome{
				Scenario: s.Name, RunID: "run-1", Status: "SUCCEEDED", StopReason: "COMPLETED",
				StepCount: 5, ToolCallCount: 3, PromptTokens: 100, CompletionTokens: 10,
				Elapsed: 12 * time.Second, AffectedService: "payment-service",
				RootCause: "The card processor is slow.",
				Steps:     []Step{{Number: 1, ActionType: "retrieve", Status: "OK", Tool: "search_knowledge"}},
			}},
		{Scenario: s, Attempt: 2, Score: Score{ServiceOK: true, Why: "the diagnosis rests on cpu satur"},
			Outcome: Outcome{
				Scenario: s.Name, RunID: "run-2", Status: "SUCCEEDED", StopReason: "MAX_STEPS",
				StepCount: 8, ToolCallCount: 6, PromptTokens: 200, CompletionTokens: 20,
				Elapsed: 30 * time.Second, AffectedService: "payment-service",
				RootCause: "payment-service is CPU saturated.\nSecond line.",
			}},
	}

	out := Report(results, Budget{MaxSteps: 12, MaxToolCalls: 10}, "test-model", time.Unix(0, 0))

	for _, want := range []string{
		"1 of 2 runs correct (50%)",
		"`payment-latency`",
		"test-model",
		"12 steps / 10 tool calls",
		"the diagnosis rests on cpu satur",
		"Run 1 — correct",
		"Run 2 — wrong",
		"search_knowledge",
		// Both individual values, because a mean would hide the difference.
		"5 / 8",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not contain %q", want)
		}
	}
	// A multi-line diagnosis must not break the blockquote.
	if strings.Contains(out, "> payment-service — payment-service is CPU saturated.\nSecond") {
		t.Error("a multi-line diagnosis was not flattened")
	}
}

func scenarioNamed(t *testing.T, name string) Scenario {
	t.Helper()
	for _, s := range EvalScenarios() {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no scenario called %s", name)
	return Scenario{}
}
