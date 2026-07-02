//go:build e2e

package e2e

import (
	"path/filepath"
	"testing"
)

// TestAssertTerminatesWithoutRetry: a stage calling martian.exit() must fail
// the run after exactly one attempt with exit 42 — the shim's ASSERT exit code
// that the generated errorStrategy maps to 'terminate'. (Port of the
// assert_min half of failure_paths.sh.)
func TestAssertTerminatesWithoutRetry(t *testing.T) {
	requireTools(t, "nextflow", "java", "python3")

	proj := transpile(t, "assert_min")

	if err := runNextflow(t, proj, "-with-trace", "trace.txt"); err == nil {
		t.Fatal("run succeeded; an assertion must fail the pipeline")
	}

	boom := attempts(readTrace(t, filepath.Join(proj, "trace.txt")), "BOOM")
	if len(boom) != 1 {
		t.Fatalf("%d BOOM attempts, want 1 (an ASSERT must terminate, not retry): %+v", len(boom), boom)
	}

	if boom[0].Exit != 42 {
		t.Errorf("BOOM exit = %d, want 42 (the ASSERT exit-code contract)", boom[0].Exit)
	}
}

// TestOrdinaryFailureRetriesWithEscalatedMemory: a stage that fails while its
// allocation is 1 GB and succeeds once escalated must be retried and see
// mem_gb == 2 — proving both the retry and the memory * task.attempt growth
// reached the stage. (Port of the flaky_retry half of failure_paths.sh.)
func TestOrdinaryFailureRetriesWithEscalatedMemory(t *testing.T) {
	requireTools(t, "nextflow", "java", "python3")

	proj := transpile(t, "flaky_retry")

	if err := runNextflow(t, proj, "-with-trace", "trace.txt"); err != nil {
		t.Fatalf("run failed; the retry should have recovered it: %v", err)
	}

	var outs struct {
		MemGB float64 `json:"mem_gb"`
	}

	readJSON(t, filepath.Join(proj, "results", "pipeline_outs.json"), &outs)

	if outs.MemGB != 2 {
		t.Errorf("mem_gb = %v, want 2 (attempt-2 escalated allocation)", outs.MemGB)
	}

	flaky := attempts(readTrace(t, filepath.Join(proj, "trace.txt")), "FLAKY")
	if len(flaky) != 2 {
		t.Errorf("%d FLAKY attempts, want 2 (fail once, succeed on retry): %+v", len(flaky), flaky)
	}
}
