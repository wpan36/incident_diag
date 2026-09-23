// Command agent-eval runs the agent against every fault the Incident Lab can
// produce and writes docs/agent-eval.md.
//
//	make up-lab && make agent-eval
//
// It is a command rather than a test, for the reason cmd/lab-scenario is one:
// it produces a report to read, and a test that fails because a model had an
// off day is a test nobody keeps.
//
// cmd/ingestion-worker and cmd/agent-worker must not be running — it joins
// their consumer groups. Each run is a billed investigation.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/wpan36/incident_diag/internal/e2e"
	"github.com/wpan36/incident_diag/internal/log"
	"github.com/wpan36/incident_diag/internal/shutdown"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// labContainers are the two services a scenario breaks. Their names are pinned
// in deploy/docker-compose.yml, because cAdvisor labels by container name.
var labContainers = []string{"checkout-service", "payment-service"}

// restartLab gives the next scenario services whose counters start at zero.
//
// Through docker rather than an HTTP call, because the lab has no reset
// endpoint and adding one would mean rebuilding a live registry — a restart is
// the mechanism internal/lab already documents ("a restarted container is a
// silently healthy one").
func restartLab(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "docker", append([]string{"restart", "--time", "5"}, labContainers...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("restarting the lab (is `make up-lab` running?): %w: %s",
			err, strings.TrimSpace(string(out)))
	}

	// Waited for here rather than left to the driver: a scenario that injects
	// a fault into a service still starting up injects it into nothing.
	deadline := time.Now().Add(60 * time.Second)
	for _, url := range []string{
		envOr("CHECKOUT_URL", "http://127.0.0.1:8081") + "/health",
		envOr("PAYMENT_URL", "http://127.0.0.1:8082") + "/health",
	} {
		for {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				return err
			}
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					break
				}
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("%s did not come back after a restart", url)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		}
	}
	return nil
}

// resultsPath is the report's path with a .json extension, so the two travel
// together and neither needs a flag of its own.
func resultsPath(report string) string {
	return strings.TrimSuffix(report, filepath.Ext(report)) + ".json"
}

func writeResults(path string, run e2e.Run) error {
	body, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding the results: %w", err)
	}
	if err := os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// rescoreFile re-marks a saved run against the current expectations.
//
// The scenarios come from EvalScenarios by name rather than from the file, so
// that a changed expectation is applied; everything else — the outcomes, the
// budget, the model, the date — is the run's as it happened.
func rescoreFile(in, out string) error {
	body, err := os.ReadFile(in)
	if err != nil {
		return fmt.Errorf("reading %s: %w", in, err)
	}
	var run e2e.Run
	if err := json.Unmarshal(body, &run); err != nil {
		return fmt.Errorf("decoding %s: %w", in, err)
	}

	byName := map[string]e2e.Scenario{}
	for _, s := range e2e.EvalScenarios() {
		byName[s.Name] = s
	}

	correct := 0
	for i := range run.Results {
		s, ok := byName[run.Results[i].Scenario.Name]
		if !ok {
			return fmt.Errorf("%s holds a scenario called %q, which no longer exists",
				in, run.Results[i].Scenario.Name)
		}
		run.Results[i].Scenario = s
		run.Results[i].Score = s.Expect.Mark(run.Results[i].Outcome)
		if run.Results[i].Score.Correct {
			correct++
		}
	}

	if err := writeResults(in, run); err != nil {
		return err
	}
	if err := os.WriteFile(out, []byte(e2e.Report(run.Results, run.Budget, run.Model, run.When)), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", out, err)
	}
	fmt.Fprintf(os.Stderr, "re-scored %d runs: %d correct — wrote %s\n", len(run.Results), correct, out)
	return nil
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func run() error {
	runs := flag.Int("runs", 3, "investigations per scenario")
	restart := flag.Bool("restart-lab", true, "restart the lab services between scenarios")
	only := flag.String("only", "", "a comma-separated subset of scenario names")
	out := flag.String("out", "docs/agent-eval.md", "where to write the report")
	rescore := flag.String("rescore", "", "re-mark a saved results file and rewrite the report, running nothing")
	flag.Parse()

	// Re-scoring costs nothing and runs nothing. The marks are a pure function
	// of the outcome, so a change to the keywords should not mean paying for
	// twenty-four investigations again — which it did once, before this
	// existed.
	if *rescore != "" {
		return rescoreFile(*rescore, *out)
	}

	logger := log.New(os.Stderr, slog.LevelWarn, "agent-eval")

	scenarios := e2e.EvalScenarios()
	if *only != "" {
		var picked []e2e.Scenario
		wanted := strings.Split(*only, ",")
		for _, name := range wanted {
			name = strings.TrimSpace(name)
			found := false
			for _, s := range scenarios {
				if s.Name == name {
					picked = append(picked, s)
					found = true
				}
			}
			if !found {
				return fmt.Errorf("no scenario called %q", name)
			}
		}
		scenarios = picked
	}

	// Signals become a cancelled context, so a Ctrl-C clears the fault through
	// the harness's own cleanup rather than leaving the lab broken.
	ctx, stop := shutdown.Context(context.Background())
	defer stop()

	storage, err := os.MkdirTemp("", "agent-eval-*")
	if err != nil {
		return fmt.Errorf("creating a document directory: %w", err)
	}
	defer os.RemoveAll(storage)

	cfg, err := e2e.ConfigFromEnv(storage, filepath.Join("testdata", "knowledge"), logger)
	if err != nil {
		return fmt.Errorf("%w (run through `make agent-eval`, which loads .env)", err)
	}

	h, err := e2e.New(ctx, cfg)
	if err != nil {
		return err
	}
	defer h.Close()

	started := time.Now()
	fmt.Fprintf(os.Stderr, "%d scenarios × %d runs against %s\n\n",
		len(scenarios), *runs, cfg.LLM.Model)

	var results []e2e.Result
	for i, s := range scenarios {
		// A fresh lab per scenario. Wiping Prometheus is not enough on its own:
		// the services keep their counters in memory, so the next scenario
		// would read this one's checkout_payment_client_timeouts_total as if
		// it had just happened — which is exactly what made an earlier run
		// report, correctly, that a healthy system was broken.
		if *restart {
			fmt.Fprintf(os.Stderr, "[%d/%d] %s — restarting the lab\n", i+1, len(scenarios), s.Name)
			if err := restartLab(ctx); err != nil {
				return err
			}
		}
		fmt.Fprintf(os.Stderr, "[%d/%d] %s — injecting and warming up\n", i+1, len(scenarios), s.Name)

		outcomes, err := h.RunRepeated(ctx, s, *runs)
		if err != nil {
			return fmt.Errorf("%s: %w", s.Name, err)
		}
		for j, o := range outcomes {
			score := s.Expect.Mark(o)
			results = append(results, e2e.Result{Scenario: s, Attempt: j + 1, Outcome: o, Score: score})

			mark := "wrong"
			if score.Correct {
				mark = "ok"
			}
			fmt.Fprintf(os.Stderr, "        run %d: %-5s %s/%s  steps=%d tools=%d  %s\n",
				j+1, mark, o.Status, o.StopReason, o.StepCount, o.ToolCallCount, score.Why)
		}
	}

	budget := e2e.Budget{MaxSteps: cfg.Agent.MaxSteps, MaxToolCalls: cfg.Agent.MaxToolCalls}
	saved := e2e.Run{Results: results, Budget: budget, Model: cfg.LLM.Model, When: time.Now()}
	if err := writeResults(resultsPath(*out), saved); err != nil {
		return err
	}
	if err := os.WriteFile(*out, []byte(e2e.Report(results, budget, saved.Model, saved.When)), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", *out, err)
	}

	correct := 0
	for _, r := range results {
		if r.Score.Correct {
			correct++
		}
	}
	fmt.Fprintf(os.Stderr, "\n%d/%d correct in %s — wrote %s\n",
		correct, len(results), time.Since(started).Round(time.Second), *out)
	return nil
}
