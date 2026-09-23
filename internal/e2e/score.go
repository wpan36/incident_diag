package e2e

import "strings"

// Expectation is how one scenario's diagnosis is marked.
//
// Keywords rather than a second model. An LLM judge tolerates paraphrase and
// makes the score itself non-deterministic — the same output could be marked
// twice and differently, and a report nobody can reproduce is not evidence.
// The cost is stated where it is paid: a run that says "the downstream card
// authorizer is slow" without the word "processor" is marked wrong, which is
// why every trajectory is recorded next to the mark.
type Expectation struct {
	// Service is what affected_service must name. Empty means the right
	// answer is that nothing is wrong.
	Service string

	// Any is groups of alternatives: the diagnosis must contain at least one
	// term from each group. Groups rather than a flat list so that "pool" and
	// "connection pool" are one requirement rather than two.
	Any [][]string

	// Not are terms that mark a wrong root cause even when the service is
	// right — the other failure mode of the same service. A term is only held
	// against a diagnosis when it is asserted rather than ruled out; see
	// restsOn.
	Not []string

	// Healthy says this is a negative case: the diagnosis has to conclude
	// nothing is broken.
	Healthy bool
}

// healthyTerms are the ways a diagnosis says nothing is wrong.
//
// It is a list rather than one phrase because the model is not constrained
// here: finish asks for a root cause and there is none to give, so the useful
// thing to check is that it said so instead of inventing one.
var healthyTerms = []string{
	"no root cause", "nothing is wrong", "nothing appears wrong", "no incident",
	"not an incident", "no fault", "healthy", "within budget", "within its budget",
	"within slo", "within its slo", "no evidence of", "operating normally",
	"no actionable", "nothing actionable", "no degradation", "not degraded",
	"within normal", "normal range", "no problem",
}

// Score is one run's mark.
type Score struct {
	Correct bool

	// ServiceOK and CauseOK split the verdict, because naming the right
	// service and naming the right reason fail for different reasons and a
	// single boolean hides which happened.
	ServiceOK bool
	CauseOK   bool

	// Why names the first requirement that failed, so that a mark can be read
	// without re-reading the diagnosis.
	Why string
}

// Mark scores one outcome against an expectation.
//
// A run that did not reach a diagnosis is wrong rather than excluded: a
// bounded agent that never finishes is a failure of the product, not a
// missing data point.
func (e Expectation) Mark(out Outcome) Score {
	text := strings.ToLower(out.RootCause)
	service := strings.ToLower(out.AffectedService)

	if strings.TrimSpace(out.RootCause) == "" {
		return Score{Why: "the run produced no diagnosis"}
	}

	if e.Healthy {
		// The service field is not checked here. A model that says "nothing is
		// wrong with payment-service" has answered correctly and still filled
		// a required field with a service name.
		for _, term := range e.Not {
			if restsOn(text, term) {
				return Score{ServiceOK: true, Why: "invented a root cause: " + term}
			}
		}
		for _, term := range healthyTerms {
			if strings.Contains(text, term) {
				return Score{Correct: true, ServiceOK: true, CauseOK: true}
			}
		}
		return Score{ServiceOK: true, Why: "did not conclude that the system is healthy"}
	}

	s := Score{ServiceOK: strings.Contains(service, strings.ToLower(e.Service))}
	if !s.ServiceOK {
		s.Why = "named " + quoteOrNone(out.AffectedService) + ", want " + e.Service
		return s
	}

	for _, term := range e.Not {
		if restsOn(text, term) {
			s.Why = "the diagnosis rests on " + term + ", which is not this fault"
			return s
		}
	}
	for _, group := range e.Any {
		if !containsAny(text, group) {
			s.Why = "the diagnosis names none of: " + strings.Join(group, ", ")
			return s
		}
	}

	s.CauseOK = true
	s.Correct = true
	return s
}

// negations are how a diagnosis rules something out.
//
// They matter because a correct diagnosis names the alternatives it rejected:
// "checkout is shedding requests; payment-service is healthy" is right, and a
// plain substring check on "payment-service" would mark it wrong. This was not
// hypothetical — it marked three correct runs wrong before it was added.
// Only markers that reject what follows them, and only ones that would not
// appear in an ordinary assertion. "below" and "within budget" are the kind of
// thing that reads as a negation and is not: "payment latency is below the
// pool's timeout" asserts a pool problem.
var negations = []string{
	"not ", "n't ", "no ", "never ", "rather than", "instead of",
	"rules out", "rule out", "ruled out", "ruling out", "rules this out",
	"is fine", "are fine", "is healthy", "are healthy",
	"unaffected", "untouched", "without", "unlike", "neither", "nor ",
	"excludes", "excluding", "does not", "did not",
}

// negationWindow is how far back restsOn looks for a negation. A sentence in
// this corpus of diagnoses runs to a few hundred characters, and a window that
// spanned one would pick up the negation of a different claim.
const negationWindow = 90

// restsOn reports whether the diagnosis asserts term as a cause rather than
// mentioning it to reject it.
//
// The heuristic is the window before each occurrence. It is a heuristic and
// the report says so: a diagnosis that negates a term further back than the
// window, or in a following clause, is still marked as resting on it. Every
// trajectory is printed next to its mark so that a disputed one can be checked
// by hand — which is the point of printing them.
func restsOn(text, term string) bool {
	for i := 0; ; {
		j := strings.Index(text[i:], term)
		if j < 0 {
			return false
		}
		at := i + j
		start := at - negationWindow
		if start < 0 {
			start = 0
		}
		if !containsAny(text[start:at], negations) {
			return true
		}
		i = at + len(term)
	}
}

func containsAny(text string, terms []string) bool {
	for _, t := range terms {
		if strings.Contains(text, t) {
			return true
		}
	}
	return false
}

func quoteOrNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "nothing"
	}
	return `"` + s + `"`
}

// EvalScenarios are M32's eight cases. Every one presents as "checkout
// requests are bad"; they differ only in why, which is what makes the set
// measure anything. Four point at payment-service and three at
// checkout-service, so naming the service is a third of the answer.
//
// The two negative cases are deliberate: an evaluation with no way to be wrong
// by inventing something measures only enthusiasm.
func EvalScenarios() []Scenario {
	symptom504 := "Customers are seeing checkout fail. POST /orders on checkout-service is " +
		"returning 504 for a large share of requests. Nothing was deployed today."
	symptomSlow := "Checkout has become slow over the last fifteen minutes. POST /orders and " +
		"GET /orders/{id} both take seconds. Nothing was deployed today."
	symptom5xx := "Checkout is failing for some customers: POST /orders returns a 5xx for a " +
		"share of requests, immediately rather than after a wait. Nothing was deployed today."
	// payment-cpu gets its own wording. It degrades rather than times out —
	// payment's p99 lands near its budget rather than past checkout's
	// two-second timeout — and an incident claiming a 504 storm sends the
	// agent hunting for one that is not there, which is how an earlier run
	// reported that the metrics contradicted the report.
	symptomSlowTimeouts := "Checkout has become slow: POST /orders is taking seconds and a " +
		"few requests are timing out. Nothing was deployed today."

	return []Scenario{
		{
			Name: "payment-latency", Fault: "payment-latency",
			Title: "Checkout is timing out on payment", Description: symptom504,
			Service: "checkout-service", ExpectService: "payment-service",
			Expect: Expectation{
				Service: "payment-service",
				// The processor is the cause and the pool is the consequence.
				// Either is accepted as naming the right mechanism; what is
				// refused is blaming checkout's timeout or payment's CPU.
				Any: [][]string{{"processor", "pool", "downstream", "connection"}},
				Not: []string{"cpu satur", "cpu throttl", "timeout is too short", "raise the timeout"},
			},
		},
		{
			Name: "payment-cpu", Fault: "payment-cpu",
			Title: "Checkout is timing out on payment", Description: symptomSlowTimeouts,
			Service: "checkout-service", ExpectService: "payment-service",
			Expect: Expectation{
				Service: "payment-service",
				Any:     [][]string{{"cpu", "saturat", "throttl"}},
				Not:     []string{"card processor is slow", "processor latency is high"},
			},
		},
		{
			Name: "payment-errors", Fault: "payment-errors",
			Title: "Checkout is failing on payment", Description: symptom5xx,
			Service: "checkout-service", ExpectService: "payment-service",
			Expect: Expectation{
				Service: "payment-service",
				Any:     [][]string{{"5xx", "503", "error rate", "errors", "rejecting", "shedding"}},
				Not:     []string{"cpu satur", "pool exhaust", "processor latency"},
			},
		},
		{
			Name: "checkout-latency", Fault: "checkout-latency",
			Title: "Checkout is slow", Description: symptomSlow,
			Service: "checkout-service", ExpectService: "checkout-service",
			Expect: Expectation{
				Service: "checkout-service",
				Any:     [][]string{{"checkout"}},
				// The discriminator against checkout-cpu: the CPU is idle, the
				// handler is waiting.
				Not: []string{"cpu satur", "cpu throttl", "payment-service is slow"},
			},
		},
		{
			Name: "checkout-cpu", Fault: "checkout-cpu",
			Title: "Checkout is slow", Description: symptomSlow,
			Service: "checkout-service", ExpectService: "checkout-service",
			Expect: Expectation{
				Service: "checkout-service",
				Any:     [][]string{{"cpu", "saturat", "throttl"}},
				Not:     []string{"payment-service is slow", "pool exhaust"},
			},
		},
		{
			Name: "checkout-errors", Fault: "checkout-errors",
			Title: "Checkout is returning errors", Description: symptom5xx,
			Service: "checkout-service", ExpectService: "checkout-service",
			Expect: Expectation{
				Service: "checkout-service",
				Any:     [][]string{{"5xx", "503", "error rate", "errors", "rejecting", "shedding"}},
				// Not "payment-service": the Service check already requires
				// checkout-service, and a correct diagnosis names
				// payment-service in order to rule it out.
				Not: []string{"cpu satur", "pool exhaust"},
			},
		},
		{
			Name: "baseline", Fault: "",
			Title: "Possible slowdown on checkout",
			Description: "A customer reported one slow checkout. Nothing is alerting and the " +
				"dashboards look normal. Please confirm whether anything is actually wrong.",
			Service: "checkout-service", ExpectService: "",
			Expect: Expectation{
				Healthy: true,
				Not:     []string{"pool exhaust", "cpu satur", "processor is slow", "error rate is high"},
			},
		},
		{
			Name: "payment-latency-mild", Fault: "payment-latency-mild",
			Title: "Checkout feels a little slower",
			Description: "A few customers said checkout felt slow this afternoon. No alerts " +
				"fired and no errors were reported. Is anything actually wrong?",
			Service: "checkout-service", ExpectService: "",
			Expect: Expectation{
				Healthy: true,
				Not:     []string{"pool exhaust", "cpu satur", "error rate is high", "outage"},
			},
		},
	}
}
