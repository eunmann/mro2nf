//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// nativeRunnerFixtures is the -native-runner golden set (#79): each transpiles
// with the flag, must reference NO martian_shell.py and NO `mre <phase>` on
// any Python stage line, and must produce its committed mrp golden. The mix
// covers a plain stage with file output (file_min), the split trio +
// chunk/join resources (split_test, join_resources), the martian service API
// surface — make_path/log/alarm/progress/allocations (api_smoke), and a
// fused/mapped kitchen sink (kitchen_sink).
var nativeRunnerFixtures = []struct{ fixture, golden string }{
	{"file_min", "expected/outs.json"},
	{"split_test", "expected/SUM_SQUARE_PIPELINE/fork0/_outs"},
	{"join_resources", "expected/outs.json"},
	{"api_smoke", "expected/outs.json"},
	{"kitchen_sink", "expected/main_outs.json"},
	// py + exec + comp in one pipeline: the runner takes ONLY the py stage;
	// exec/comp keep their adapter launchers — per-language routing retained.
	{"mixed_adapters", "expected/outs.json"},
}

// TestNativeRunner guards #79: with -native-runner a Python stage's
// split/main/join run through the embedded run_stage.py + martian compat shim
// — no martian_shell.py adapter and no mre broker on the stage-execution hop —
// with output identical to the committed mrp golden.
func TestNativeRunner(t *testing.T) {
	requireTools(t, "nextflow", "java", "python3")

	for _, tc := range nativeRunnerFixtures {
		t.Run(tc.fixture, func(t *testing.T) {
			t.Parallel()

			proj := transpile(t, tc.fixture, "-native-runner")
			assertNoAdapterOnPyStages(t, proj)

			if err := runNextflow(t, proj); err != nil {
				t.Fatal(err)
			}

			goldenJSON(t,
				filepath.Join(proj, "results", "pipeline_outs.json"),
				filepath.Join(root, "testdata", tc.fixture, tc.golden))
		})
	}
}

// assertNoAdapterOnPyStages fails if any generated module still runs a PYTHON
// stage through the adapter: no `-lang py` line may carry the -shell adapter
// broker. comp/exec stages legitimately keep martian_shell.py + mre (the
// runner is py-only), and mre may still appear for the data plane —
// bind/forkbind/merge — which is #76 territory.
func assertNoAdapterOnPyStages(t *testing.T, proj string) {
	t.Helper()

	mods, err := filepath.Glob(filepath.Join(proj, "modules", "*.nf"))
	if err != nil {
		t.Fatal(err)
	}

	for _, f := range append(mods, filepath.Join(proj, "main.nf")) {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}

		for _, line := range strings.Split(string(data), "\n") {
			if strings.Contains(line, "-lang py") && strings.Contains(line, "-shell") {
				t.Errorf("%s: py stage still brokered by the adapter: %s", filepath.Base(f), strings.TrimSpace(line))
			}
		}
	}

	if _, err := os.Stat(filepath.Join(proj, "_assets", "runner", "run_stage.py")); err != nil {
		t.Errorf("runner asset missing: %v", err)
	}
}

// TestNativeRunnerComposesWithNative proves the two opt-ins stack: -native
// collapses the orchestration (scatter + merge fold, zero data-plane tasks)
// while -native-runner swaps the stage hop, and the combined project still
// produces output byte-identical to the default run.
func TestNativeRunnerComposesWithNative(t *testing.T) {
	requireTools(t, "nextflow", "java", "python3")

	for _, fx := range []string{"fork_min", "split_test"} {
		t.Run(fx, func(t *testing.T) {
			t.Parallel()

			combined := transpile(t, fx, "-native", "-native-runner")
			assertNoAdapterOnPyStages(t, combined)

			def := transpile(t, fx)
			if err := runNextflow(t, combined); err != nil {
				t.Fatal(err)
			}
			if err := runNextflow(t, def); err != nil {
				t.Fatal(err)
			}

			goldenJSON(t,
				filepath.Join(combined, "results", "pipeline_outs.json"),
				filepath.Join(def, "results", "pipeline_outs.json"))
		})
	}
}

// TestNativeRunnerExitContract re-proves the assert/retry contract through the
// runner: an ASSERT (martian.exit) terminates after one attempt with exit 42;
// an ordinary failure retries with escalated memory and the shim's
// get_memory_allocation reports the attempt-2 value.
func TestNativeRunnerExitContract(t *testing.T) {
	requireTools(t, "nextflow", "java", "python3")

	t.Run("assert terminates", func(t *testing.T) {
		t.Parallel()

		proj := transpile(t, "assert_min", "-native-runner")
		if err := runNextflow(t, proj, "-with-trace", "trace.txt"); err == nil {
			t.Fatal("run succeeded; an assertion must fail the pipeline")
		}

		boom := attempts(readTrace(t, filepath.Join(proj, "trace.txt")), "BOOM")
		if len(boom) != 1 {
			t.Fatalf("%d BOOM attempts, want 1 (ASSERT terminates): %+v", len(boom), boom)
		}
		if boom[0].Exit != 42 {
			t.Errorf("BOOM exit = %d, want 42", boom[0].Exit)
		}
	})

	t.Run("ordinary failure retries", func(t *testing.T) {
		t.Parallel()

		proj := transpile(t, "flaky_retry", "-native-runner")
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
			t.Errorf("%d FLAKY attempts, want 2: %+v", len(flaky), flaky)
		}
	})
}
