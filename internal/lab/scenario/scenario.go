// Package scenario drives the Incident Lab into the failures the knowledge
// corpus describes, and reports whether the numbers moved the way the runbook
// for that failure says they should.
//
// It exists so that a failure can be reproduced on purpose rather than waited
// for, and so that the agent evaluation in M32 has a fixed set of incidents
// with known answers — including one where the answer is "nothing is wrong".
package scenario

import (
	"fmt"
	"strings"
	"time"

	"github.com/wpan36/incident_diag/internal/lab"
)

// Services, as a fault call names them.
const (
	targetCheckout = "checkout-service"
	targetPayment  = "payment-service"
)

// paymentCPUWorkers is what the payment-cpu scenario spins.
//
// It is the lab's maximum rather than DefaultParams().Workers because
// payment-service is hard to starve: it is limited to one CPU, Go derives
// GOMAXPROCS from that, and its handler mostly waits on the simulated
// processor. Two spinners leave it answering in 87ms — no incident at all,
// which is what an evaluation run measured and correctly reported.
const paymentCPUWorkers = lab.MaxCPUWorkers

// Params are the fault settings a scenario builds its request from. They are
// flags because the values that reproduce a shape depend on the machine.
type Params struct {
	DelayMS  int
	JitterMS int
	Status   int
	Ratio    float64
	Workers  int
}

// DefaultParams reproduce the shapes described in
// docs/plans/observability-and-fault-scenarios.md at the default request rate.
func DefaultParams() Params {
	return Params{
		// Past payment-service's 2s latency alert and past checkout-service's
		// 2s client timeout, so both the latency incident and the 504s follow.
		DelayMS:  2500,
		JitterMS: 250,
		Status:   503,
		Ratio:    0.2,
		Workers:  2,
	}
}

// faultCall is one POST /fault.
type faultCall struct {
	target string
	body   map[string]any
}

// Scenario is one reproducible failure.
type Scenario struct {
	Name string

	// Summary is one line: what is injected.
	Summary string

	// Expect is the shape the run should produce, in the terms the runbook
	// uses. It is printed with the report so that a run can be read against it.
	Expect string

	// Runbook names the documents in testdata/knowledge that diagnose this.
	Runbook string

	// Fault builds the call that starts the incident. A nil Fault is the
	// healthy control.
	Fault func(Params) faultCall
}

// All returns every scenario, in the order they are worth running.
func All() []Scenario {
	return []Scenario{
		{
			Name:    "baseline",
			Summary: "no fault: the lab under normal load",
			Expect: "everything 2xx; payment_pool_in_use well below payment_pool_size; " +
				"no entries in checkout_payment_client_timeouts_total",
			Runbook: "none — this is the negative case, where the right diagnosis is that nothing is wrong",
		},
		{
			Name:    "payment-latency",
			Summary: "the card processor slows down, inside payment-service's connection pool",
			Expect: "payment_processor_latency_seconds rises and payment_pool_in_use / payment_pool_size " +
				"follows it towards 1, while payment_pool_wait_seconds stays low — which is the runbook's " +
				"discriminator: the pool is saturated as a consequence, so raising its size is the wrong " +
				"remediation. payment-service's own p99 passes its 2s alert, checkout-service answers 504 " +
				"and checkout_payment_client_timeouts_total rises, and payment-service's 5xx rate stays flat",
			Runbook: "payment-latency-runbook.md, then connection-pool-runbook.md",
			Fault: func(p Params) faultCall {
				return faultCall{target: targetPayment, body: map[string]any{
					"kind": "latency", "enabled": true,
					"delay_ms": p.DelayMS, "jitter_ms": p.JitterMS,
				}}
			},
		},
		{
			Name:    "checkout-cpu",
			Summary: "checkout-service saturates its own CPU",
			Expect: "latency rises on POST /orders and on GET /orders/:id, which calls nothing downstream; " +
				"payment-service is untouched; rate(process_cpu_seconds_total{job=\"checkout-service\"}[1m]) rises, " +
				"and container_cpu_cfs_throttled_seconds_total does too where cAdvisor has data",
			Runbook: "cpu-saturation-runbook.md, and the CPU section of checkout-latency-runbook.md",
			Fault: func(p Params) faultCall {
				return faultCall{target: targetCheckout, body: map[string]any{
					"kind": "cpu", "enabled": true, "workers": p.Workers,
				}}
			},
		},
		{
			Name:    "checkout-latency",
			Summary: "checkout-service is slow in its own handler, with payment-service healthy",
			Expect: "checkout-service's p99 rises on every route including GET /orders/:id, which calls " +
				"nothing downstream, while payment-service's own p99 and its pool stay flat and no " +
				"checkout_payment_client_timeouts_total appear. The discriminator against checkout-cpu is " +
				"that process_cpu_seconds_total{job=\"checkout-service\"} does not rise: checkout is waiting, " +
				"not working",
			Runbook: "checkout-latency-runbook.md",
			Fault: func(p Params) faultCall {
				return faultCall{target: targetCheckout, body: map[string]any{
					"kind": "latency", "enabled": true,
					"delay_ms": p.DelayMS, "jitter_ms": p.JitterMS,
				}}
			},
		},
		{
			Name:    "payment-cpu",
			Summary: "payment-service saturates its own CPU",
			Expect: "payment-service is slow and checkout-service answers 504, which looks exactly like " +
				"payment-latency from checkout's side. The discriminator is inside payment-service: " +
				"payment_processor_latency_seconds stays flat — the card processor is fine — while " +
				"rate(process_cpu_seconds_total{job=\"payment-service\"}[1m]) rises",
			Runbook: "cpu-saturation-runbook.md, then payment-latency-runbook.md for what it is not",
			Fault: func(Params) faultCall {
				// Not Params.Workers, which is checkout's number. payment-service
				// needs far more spinners to show the same shape, because its
				// handler spends most of its time asleep inside the simulated
				// processor call rather than computing: at two workers it stays
				// under 100ms and there is no incident to find. checkout's
				// handler serializes orders, which is real CPU work, so two is
				// enough there.
				return faultCall{target: targetPayment, body: map[string]any{
					"kind": "cpu", "enabled": true, "workers": paymentCPUWorkers,
				}}
			},
		},
		{
			Name:    "checkout-errors",
			Summary: "checkout-service sheds a share of its own requests",
			Expect: "checkout-service's 5xx rate matches the injected ratio while its latency stays flat " +
				"and payment-service is untouched — no 502, no 504, no client timeouts. The fault is " +
				"in the service the incident was filed against, which is the case the corpus warns is " +
				"the least likely",
			Runbook: "error-rate-runbook.md",
			Fault: func(p Params) faultCall {
				return faultCall{target: targetCheckout, body: map[string]any{
					"kind": "error", "enabled": true,
					"status": p.Status, "ratio": p.Ratio,
				}}
			},
		},
		{
			Name:    "payment-latency-mild",
			Summary: "payment-service is a little slower than usual, and nothing is actually failing",
			Expect: "payment-service's p99 rises to a few hundred milliseconds — visible, but under its " +
				"2s alert and far under checkout-service's 2s client timeout, so every request still " +
				"succeeds. The right answer is that nothing is broken, which is what makes this the " +
				"second negative case: a run that invents a root cause here is wrong in the way that " +
				"matters most",
			Runbook: "none — the right diagnosis is that the numbers are within budget",
			Fault: func(Params) faultCall {
				// Fixed rather than taken from Params: the whole point is a
				// delay under both thresholds, and a scenario whose meaning
				// depends on a flag would not be the negative case.
				return faultCall{target: targetPayment, body: map[string]any{
					"kind": "latency", "enabled": true,
					"delay_ms": 300, "jitter_ms": 100,
				}}
			},
		},
		{
			Name:    "payment-errors",
			Summary: "payment-service sheds a share of requests",
			Expect: "payment-service's 5xx rate matches the injected ratio while its latency stays flat; " +
				"checkout-service answers 502 rather than 504, because it received an error rather than giving up",
			Runbook: "error-rate-runbook.md",
			Fault: func(p Params) faultCall {
				return faultCall{target: targetPayment, body: map[string]any{
					"kind": "error", "enabled": true,
					"status": p.Status, "ratio": p.Ratio,
				}}
			},
		},
	}
}

// Find returns the scenario with this name.
func Find(name string) (Scenario, error) {
	for _, s := range All() {
		if s.Name == name {
			return s, nil
		}
	}
	return Scenario{}, fmt.Errorf("no scenario called %q; try one of %s", name, strings.Join(Names(), ", "))
}

// Names returns every scenario name.
func Names() []string {
	all := All()
	names := make([]string, len(all))
	for i, s := range all {
		names[i] = s.Name
	}
	return names
}

// List renders the scenario table.
func List() string {
	var b strings.Builder
	for _, s := range All() {
		fmt.Fprintf(&b, "%s\n  fault:   %s\n  expect:  %s\n  runbook: %s\n\n",
			s.Name, s.Summary, s.Expect, s.Runbook)
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// phase is one segment of a run.
type phase struct {
	name     string
	duration time.Duration
}
