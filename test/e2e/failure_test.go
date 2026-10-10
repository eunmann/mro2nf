//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInvalidOutputFailsLikeMrp: a stage whose struct output lacks a declared
// member fails at its own output validation, as real mrp fails the stage with
// "Expected S output value 's' missing key: b" (#274), on the adapter path and
// under the native runner alike. The consumer projecting UP.s.b never runs, so
// the missing key is never bound as null.
func TestInvalidOutputFailsLikeMrp(t *testing.T) {
	requireTools(t, "nextflow", "java", "python3")

	for _, tc := range []struct {
		name  string
		flags []string
	}{
		{"adapter", nil},
		{"native_runner", []string{"-native-runner"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proj := transpile(t, "missing_struct_key", tc.flags...)

			if err := runNextflow(t, proj, "-with-trace", "trace.txt"); err == nil {
				t.Fatal("run succeeded; a stage output missing a declared struct key must fail it")
			}

			log, err := os.ReadFile(filepath.Join(proj, "run.log"))
			if err != nil {
				t.Fatalf("read run.log: %v", err)
			}

			if !strings.Contains(string(log), "Expected S output value 's' missing key: b") {
				t.Errorf("run.log does not name mrp's validation failure:\n%s", log)
			}

			if down := attempts(readTrace(t, filepath.Join(proj, "trace.txt")), "DOWN"); len(down) != 0 {
				t.Errorf("DOWN ran %d time(s); it must never see UP's invalid output: %+v", len(down), down)
			}
		})
	}
}

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

// TestStageOutputLinkOutsideWorkFails guards #332: a stage whose file output is
// a symlink to a file outside its task work dir must fail the run, naming the
// output and the target, under both runners, rather than staging the target
// into its bundle and publishing it.
func TestStageOutputLinkOutsideWorkFails(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java", "python3")

	for name, flags := range map[string][]string{"mre": nil, "native_runner": {"-native-runner"}} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			proj := transpile(t, "symlink_escape", flags...)

			if err := runNextflow(t, proj); err == nil {
				t.Fatal("run succeeded; an output linked outside the work dir must fail it")
			}

			log, err := os.ReadFile(filepath.Join(proj, "run.log"))
			if err != nil {
				t.Fatal(err)
			}

			if !strings.Contains(string(log), "outside the task work dir") ||
				!strings.Contains(string(log), "linked") ||
				!strings.Contains(string(log), filepath.Join("stages", "link_out", "__init__.py")) {
				t.Errorf("run log must name the output and its target:\n%s", tail(log, 40))
			}

			if fileExists(filepath.Join(proj, "results", "linked.txt")) {
				t.Error("the link target was published")
			}
		})
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
