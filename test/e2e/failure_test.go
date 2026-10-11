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

// runnerLegs are the transpile flags of the two stage runners.
var runnerLegs = map[string][]string{"mre": nil, "native_runner": {"-native-runner"}}

// TestStageOutputLinkOutsideWorkFails guards #332 on isolated container
// targets (-confine-outputs, implied by awsbatch/healthomics): a stage whose
// file output is a symlink to a file outside its task work dir must fail the
// run, naming the output and the target, under both runners, rather than
// staging the target into its bundle and uploading it.
func TestStageOutputLinkOutsideWorkFails(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java", "python3")

	for name, flags := range runnerLegs {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			proj := transpile(t, "symlink_escape", append([]string{"-confine-outputs"}, flags...)...)

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

// TestStageOutputLinkOutsideWorkCopiedLocally is the local-target side of
// #332: on a shared filesystem mrp publishes the symlink, so the run succeeds
// and publishes the target's content, but the stage's output bundle holds a
// copy of the host file, never a hard link that a later write could alias.
func TestStageOutputLinkOutsideWorkCopiedLocally(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java", "python3")

	host := filepath.Join(root, "testdata", "symlink_escape", "stages", "link_out", "__init__.py")

	want, err := os.ReadFile(host)
	if err != nil {
		t.Fatal(err)
	}

	hostInfo, err := os.Stat(host)
	if err != nil {
		t.Fatal(err)
	}

	for name, flags := range runnerLegs {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			proj := transpile(t, "symlink_escape", flags...)
			if err := runNextflow(t, proj); err != nil {
				t.Fatal(err)
			}

			if got, err := os.ReadFile(filepath.Join(proj, "results", "linked.txt")); err != nil || string(got) != string(want) {
				t.Errorf("published linked.txt = %q (err %v), want the link target's content", got, err)
			}

			leaves, err := filepath.Glob(filepath.Join(proj, "work", "*", "*", "outs*", "f", "L0000.txt"))
			if err != nil || len(leaves) == 0 {
				t.Fatalf("no staged output leaf in the work dir (err %v)", err)
			}

			for _, leaf := range leaves {
				info, err := os.Stat(leaf)
				if err != nil {
					t.Fatal(err)
				}

				if os.SameFile(info, hostInfo) {
					t.Errorf("bundle leaf %s is a hard link to the host file %s; it must be a copy", leaf, host)
				}
			}
		})
	}
}

// TestOrdinaryFailureRetriesWithEscalatedMemory: under -monitor, a stage the
// RSS kill fails at its 0.5 GB request must be retried, succeed because the
// kill limit follows the escalated allocation, and still see mrp's 0.5 GB in
// _jobinfo and __mem_gb — the escalation sizes the task, never the stage
// (#243). (Port of the flaky_retry half of failure_paths.sh.)
func TestOrdinaryFailureRetriesWithEscalatedMemory(t *testing.T) {
	requireTools(t, "nextflow", "java", "python3")

	proj := transpile(t, "flaky_retry", "-monitor")

	if err := runNextflow(t, proj, "-with-trace", "trace.txt"); err != nil {
		t.Fatalf("run failed; the retry should have recovered it: %v", err)
	}

	var outs struct {
		MemGB    float64 `json:"mem_gb"`
		ArgMemGB float64 `json:"arg_mem_gb"`
	}

	readJSON(t, filepath.Join(proj, "results", "pipeline_outs.json"), &outs)

	if outs.MemGB != 0.5 || outs.ArgMemGB != 0.5 {
		t.Errorf("stage saw mem_gb=%v __mem_gb=%v, want 0.5/0.5 (mrp's request, no retry factor)", outs.MemGB, outs.ArgMemGB)
	}

	flaky := attempts(readTrace(t, filepath.Join(proj, "trace.txt")), "FLAKY")
	if len(flaky) != 2 {
		t.Errorf("%d FLAKY attempts, want 2 (fail once, succeed on retry): %+v", len(flaky), flaky)
	}
}
