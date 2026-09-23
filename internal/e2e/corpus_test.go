package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The manifest decides what each document is, not its filename.
//
// The filename guess this replaced was wrong where it mattered:
// cpu-saturation-runbook.md has no "checkout" in its name, so it was uploaded
// as payment-service when the manifest declares it unattributed because it
// applies to either. A search filtered to checkout-service could then not see
// the CPU runbook at all.
func TestLoadManifestDescribesTheWholeCorpus(t *testing.T) {
	manifest, err := loadManifest(filepath.Join("..", "..", "testdata", "knowledge"))
	if err != nil {
		t.Fatalf("loadManifest: %v", err)
	}

	cpu, ok := manifest["cpu-saturation-runbook.md"]
	if !ok {
		t.Fatal("the manifest does not list cpu-saturation-runbook.md")
	}
	if cpu.Service != nil {
		t.Errorf("the CPU runbook is attributed to %q; it applies to either service",
			*cpu.Service)
	}
	if cpu.DocumentType != "runbook" {
		t.Errorf("the CPU runbook's type = %q", cpu.DocumentType)
	}

	payment, ok := manifest["payment-latency-runbook.md"]
	if !ok {
		t.Fatal("the manifest does not list payment-latency-runbook.md")
	}
	if payment.Service == nil || *payment.Service != "payment-service" {
		t.Errorf("the payment latency runbook's service = %v", payment.Service)
	}
}

// Every scenario's expectation has to be reachable from a document the agent
// can retrieve. The payment-cpu scenario failed 3/3 in the first evaluation
// because the runbook chain for a slow payment-service ended at the pool and
// the processor and never mentioned CPU.
func TestThePaymentRunbookNamesTheCPUMetric(t *testing.T) {
	path := filepath.Join("..", "..", "testdata", "knowledge", "payment-latency-runbook.md")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the runbook: %v", err)
	}

	for _, want := range []string{
		`process_cpu_seconds_total{job="payment-service"}`,
		"container_cpu_cfs_throttled_seconds_total",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("the payment latency runbook does not name %s", want)
		}
	}
}
