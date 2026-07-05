//go:build e2e

package e2e

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestResumeCachesEverything: rerunning an unchanged, successful pipeline with
// -resume must execute ZERO new tasks (everything cached). A cache-key
// instability — e.g. a timestamp leaking into a staged asset, or a gather task
// whose input order is completion order — would otherwise ship silently and
// destroy resumability on long runs. The table pins the gather shapes that
// carry order countermeasures (#123): the -native element scatter, the keyed
// split triad's JOIN_K, the keyed nested-map MERGE_K, and the non-keyed split
// triad's JOIN (plain + fused). These runtime cases are probabilistic guards —
// an unsorted gather only re-executes when completion order happens to churn;
// the emit-side text pins are the deterministic ones. (Port of the -resume
// half of runtime_knobs.sh.)
func TestResumeCachesEverything(t *testing.T) {
	requireTools(t, "nextflow", "java", "python3")

	cases := []struct {
		name    string
		fixture string
		flags   []string
	}{
		{"diamond_min", "diamond_min", nil},
		// Default-mode non-keyed split triad: a >=2-chunk split whose JOIN
		// gathers the chunk outs sorted by name (toSortedList, not collect()).
		{"split_test", "split_test", nil},
		// The -native shapes whose gathers carry the -resume cache-key
		// countermeasures (#123): the O(1) element scatter (toSortedList over
		// bundle names), the keyed split triad (sorted JOIN_K chunk outs), and
		// the keyed nested map (_KS scatter + sorted MERGE_K gather).
		{"native_fork_min", "fork_min", []string{"-native"}},
		{"native_map_split", "map_split", []string{"-native"}},
		{"native_map_pipe_nested", "map_pipe_nested", []string{"-native"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			proj := transpile(t, tc.fixture, tc.flags...)

			if err := runNextflow(t, proj); err != nil {
				t.Fatalf("first run: %v", err)
			}

			if err := runNextflow(t, proj, "-resume", "-with-trace", "trace2.txt"); err != nil {
				t.Fatalf("-resume rerun: %v", err)
			}

			rows := readTrace(t, filepath.Join(proj, "trace2.txt"))
			if len(rows) == 0 {
				t.Fatal("empty trace on -resume rerun, want >0 rows")
			}

			// Any non-CACHED row is a task that re-executed, i.e. an unstable
			// cache key.
			for _, row := range rows {
				if row.Status != "CACHED" {
					t.Errorf("task %s re-executed under -resume (status %s), want CACHED", row.Name, row.Status)
				}
			}
		})
	}
}

// jobinfo is the slice of a stage's _jobinfo the overrides overlay must reach.
type jobinfo struct {
	MemGB   float64 `json:"memGB"`
	Threads float64 `json:"threads"`
}

// TestOverridesOverlayReachesStage: an `mro2nf overrides` -c overlay must
// actually retune the targeted stage — verified from the _jobinfo the stage
// saw, not from the generated config text. The untargeted sibling must keep
// its default. (Port of the overrides half of runtime_knobs.sh.)
func TestOverridesOverlayReachesStage(t *testing.T) {
	requireTools(t, "nextflow", "java")
	buildBinaries(t)

	work := t.TempDir()
	overridesJSON := filepath.Join(work, "overrides.json")

	if err := os.WriteFile(overridesJSON, []byte(`{ "D.GEN": { "mem_gb": 3, "threads": 2 } }`), 0o644); err != nil {
		t.Fatalf("write overrides.json: %v", err)
	}

	out, err := exec.Command(filepath.Join(root, "mro2nf"), "overrides", overridesJSON).Output()
	if err != nil {
		t.Fatalf("mro2nf overrides: %v", err)
	}

	overlay := filepath.Join(work, "overrides.config")
	if err := os.WriteFile(overlay, out, 0o644); err != nil {
		t.Fatalf("write overrides.config: %v", err)
	}

	proj := transpile(t, "diamond_min")

	if err := runNextflow(t, proj, "-c", overlay); err != nil {
		t.Fatal(err)
	}

	gen, add := stageJobinfos(t, filepath.Join(proj, "work"))

	if gen == nil || add == nil {
		t.Fatalf("GEN found=%v, ADD found=%v; want both stages' _jobinfo under work/", gen != nil, add != nil)
	}

	if gen.MemGB != 3 || gen.Threads != 2 {
		t.Errorf("GEN saw memGB=%v threads=%v, want 3/2 (override did not reach the stage)", gen.MemGB, gen.Threads)
	}

	if add.MemGB == 3 {
		t.Error("ADD saw memGB=3; the D.GEN override must not leak to an untargeted stage")
	}
}

// TestOverridesPipelineScopeReachesStages exercises the `-mro` path (#45): a
// pipeline-scoped key (naming the pipeline D, not a leaf stage) must expand to
// every stage beneath it, so BOTH GEN and ADD see the override. Without -mro the
// key would render a dead selector for the pipeline name and reach neither.
func TestOverridesPipelineScopeReachesStages(t *testing.T) {
	requireTools(t, "nextflow", "java")
	buildBinaries(t)

	work := t.TempDir()
	overridesJSON := filepath.Join(work, "overrides.json")

	if err := os.WriteFile(overridesJSON, []byte(`{ "D": { "mem_gb": 3 } }`), 0o644); err != nil {
		t.Fatalf("write overrides.json: %v", err)
	}

	fixture := filepath.Join(root, "testdata", "diamond_min")

	out, err := exec.Command(filepath.Join(root, "mro2nf"), "overrides",
		"-mro", filepath.Join(fixture, "pipeline.mro"), "-mropath", fixture, overridesJSON).Output()
	if err != nil {
		t.Fatalf("mro2nf overrides -mro: %v", err)
	}

	overlay := filepath.Join(work, "overrides.config")
	if err := os.WriteFile(overlay, out, 0o644); err != nil {
		t.Fatalf("write overrides.config: %v", err)
	}

	proj := transpile(t, "diamond_min")

	if err := runNextflow(t, proj, "-c", overlay); err != nil {
		t.Fatal(err)
	}

	gen, add := stageJobinfos(t, filepath.Join(proj, "work"))
	if gen == nil || add == nil {
		t.Fatalf("GEN found=%v, ADD found=%v; want both stages' _jobinfo", gen != nil, add != nil)
	}

	if gen.MemGB != 3 || add.MemGB != 3 {
		t.Errorf("pipeline-scoped D override reached GEN memGB=%v, ADD memGB=%v; want both 3", gen.MemGB, add.MemGB)
	}
}

// stageJobinfos walks the work tree and returns the _jobinfo the GEN and ADD
// stages saw. _jobinfo's own `name` carries the entry callable, so the stage
// is identified from the task dir's .command.run header instead.
func stageJobinfos(t *testing.T, work string) (gen, add *jobinfo) {
	t.Helper()

	walkErr := filepath.WalkDir(work, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "_jobinfo" {
			return err
		}

		// work/<xx>/<hash>/main/_jobinfo -> the task dir holds .command.run.
		runFile := filepath.Join(filepath.Dir(filepath.Dir(path)), ".command.run")

		raw, readErr := os.ReadFile(runFile)
		if readErr != nil {
			return nil // no .command.run two levels up: not a task's _jobinfo
		}

		header := commandRunHeader(raw)

		var info jobinfo

		readJSON(t, path, &info)

		switch {
		case strings.Contains(header, "__GEN"):
			gen = &info
		case strings.Contains(header, "__ADD"):
			add = &info
		}

		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk %s: %v", work, walkErr)
	}

	return gen, add
}

// commandRunHeader returns the first three lines of a .command.run script —
// enough to carry Nextflow's "NEXTFLOW TASK: <process>" banner.
func commandRunHeader(raw []byte) string {
	lines := strings.SplitN(string(raw), "\n", 4)
	if len(lines) > 3 {
		lines = lines[:3]
	}

	return strings.Join(lines, " ")
}
