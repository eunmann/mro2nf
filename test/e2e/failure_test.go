//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestInvalidOutputFailsLikeMrp: a stage whose struct output lacks a declared
// member fails at its own output validation, as real mrp fails the stage with
// "Expected S output value 's' missing key: b" (#274). The consumer projecting
// UP.s.b never runs, so the missing key is never bound as null.
func TestInvalidOutputFailsLikeMrp(t *testing.T) {
	requireTools(t, "nextflow", "java", "python3")

	proj := transpile(t, "missing_struct_key")

	if err := runNextflowAllowingFailedAttempts(t, proj, "-with-trace", "trace.txt"); err == nil {
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
}

// TestAssertTerminatesWithoutRetry: a stage calling martian.exit() must fail
// the run after exactly one attempt with exit 42 — the shim's ASSERT exit code
// that the generated errorStrategy maps to 'terminate'. (Port of the
// assert_min half of failure_paths.sh.)
func TestAssertTerminatesWithoutRetry(t *testing.T) {
	requireTools(t, "nextflow", "java", "python3")

	proj := transpile(t, "assert_min")

	if err := runNextflowAllowingFailedAttempts(t, proj, "-with-trace", "trace.txt"); err == nil {
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

// TestPreflightFailsBeforeItsPipeline: a failed preflight fails the run
// before any other call of its pipeline starts, mapped or not, as mrp makes
// the preflight a prenode of each (core/pipestance.go NewPipestance); mrp's
// run of preflight_fail runs CHECK alone (#314).
func TestPreflightFailsBeforeItsPipeline(t *testing.T) {
	requireTools(t, "nextflow", "java", "python3")

	proj := transpile(t, "preflight_fail")

	if err := runNextflowAllowingFailedAttempts(t, proj, "-with-trace", "trace.txt"); err == nil {
		t.Fatal("run succeeded; the failed preflight must fail it")
	}

	rows := readTrace(t, filepath.Join(proj, "trace.txt"))
	if ran := append(attempts(rows, "WORK"), attempts(rows, "EACH")...); len(ran) != 0 {
		t.Errorf("%d tasks of the preflight's pipeline ran: %+v", len(ran), ran)
	}

	if check := attempts(rows, "CHECK"); len(check) != 1 || check[0].Exit != 42 {
		t.Errorf("CHECK attempts %+v, want one exiting 42", check)
	}
}

// TestStageOutputLinkOutsideWorkFails guards #332 on isolated container
// targets (-confine-outputs, implied by awsbatch/healthomics): a stage whose
// file output is a symlink to a file outside its task work dir must fail the
// run, naming the output and the target, rather than staging the target into
// its bundle and uploading it.
func TestStageOutputLinkOutsideWorkFails(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java", "python3")

	proj := transpile(t, "symlink_escape", "-confine-outputs")

	if err := runNextflowAllowingFailedAttempts(t, proj); err == nil {
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

	proj := transpile(t, "symlink_escape")
	if err := runNextflow(t, proj); err != nil {
		t.Fatal(err)
	}

	if got, err := os.ReadFile(filepath.Join(proj, "results", "linked.txt")); err != nil || string(got) != string(want) {
		t.Errorf("published linked.txt = %q (err %v), want the link target's content", got, err)
	}

	var leaves []string

	for _, bundle := range []string{"stage_outs", filepath.Join("outs", "*")} {
		found, err := filepath.Glob(filepath.Join(proj, "work", "*", "*", bundle, "f", "L0000", "*"))
		if err != nil {
			t.Fatal(err)
		}

		leaves = append(leaves, found...)
	}

	if len(leaves) == 0 {
		t.Fatal("no staged output leaf in the work dir")
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
}

// TestMonitorMemoryKillFailsLikeMrp: -monitor matches mrp's --monitor
// (#473). mrjob kills FLAKY, which holds 0.75 GB, at its 0.5 GB request
// and writes "Stage exceeded its memory quota", which mrp retries only
// under --auto-adjust-memory (core/runtime.go:86 getRetryRegexps), so a
// real mrp --monitor run of flaky_retry fails FLAKY after one job. The run
// fails with FLAKY's error after one attempt.
func TestMonitorMemoryKillFailsLikeMrp(t *testing.T) {
	requireTools(t, "nextflow", "java", "python3")

	proj := transpile(t, "flaky_retry", "-monitor")

	o, names := nextflowRunOutcome(t, proj)
	if want := failedIn(failError, "FLAKY"); !o.equal(want) {
		t.Errorf("outcome %s, want mrp's %s", o, want)
	}

	if n := len(slices.DeleteFunc(names, func(s string) bool { return !strings.Contains(s, "FLAKY") })); n != 1 {
		t.Errorf("%d FLAKY attempts, want 1: mrp does not retry a memory-quota kill", n)
	}
}
