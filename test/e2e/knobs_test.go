//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestResumeCachesEverything: rerunning an unchanged, successful pipeline with
// -resume must execute ZERO new tasks (everything cached). A cache-key
// instability — e.g. a timestamp leaking into a staged asset, or a gather task
// whose input order is completion order — would otherwise ship silently and
// destroy resumability on long runs. The fixtures pin the gather shapes: a
// split triad's join of its chunk outs, a map call's gather of its forks, a
// split stage under a map call and a map call over a sub-pipeline holding
// a split.
// These runtime cases are probabilistic guards: an unsorted gather only
// re-executes when completion order happens to churn.
func TestResumeCachesEverything(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java", "python3")

	for _, fixture := range []string{"diamond_min", "split_test", "map_fork", "fork_min", "map_split", "map_pipe_split"} {
		t.Run(fixture, func(t *testing.T) {
			t.Parallel()

			proj := transpile(t, fixture)

			if err := runNextflow(t, proj); err != nil {
				t.Fatalf("first run: %v", err)
			}

			if err := runNextflow(t, proj, "-resume", "-with-trace", "trace2.txt"); err != nil {
				t.Fatalf("-resume rerun: %v", err)
			}

			assertAllCached(t, readTrace(t, filepath.Join(proj, "trace2.txt")))
		})
	}
}

// TestLocalClampRunsOversizedRequest pins #321: on the standard profile a stage
// requesting more cpus and memory than the host runs with the host's
// process.resourceLimits, where Nextflow's local executor would otherwise fail
// it ("Process requirement exceeds available CPUs"). The untargeted ADD keeps
// its own request.
func TestLocalClampRunsOversizedRequest(t *testing.T) {
	requireTools(t, "nextflow", "java", "python3")

	proj := transpile(t, "diamond_min")

	overlay := filepath.Join(proj, "oversized.config")
	body := "process { withName: 'N0_GEN' { cpus = 4096; memory = '100000 GB' } }\n" +
		"trace.fields = 'task_id,hash,name,status,exit,cpus,memory'\n"

	if err := os.WriteFile(overlay, []byte(body), 0o644); err != nil {
		t.Fatalf("write overlay: %v", err)
	}

	if err := runNextflow(t, proj, "-c", overlay, "-with-trace", "clamp-trace.txt"); err != nil {
		t.Fatal(err)
	}

	wantCpus, wantMem := resolvedLimits(t, proj)

	got := map[string][2]string{}
	for _, row := range readTraceColumns(t, filepath.Join(proj, "clamp-trace.txt"), "name", "cpus", "memory") {
		got[row[0]] = [2]string{row[1], row[2]}
	}

	want := map[string][2]string{
		"N0_GEN (D.GEN)": {wantCpus, wantMem},
		"N1_ADD (D.ADD)": {"1", "1 GB"},
	}

	stages := map[string][2]string{}
	for name := range want {
		stages[name] = got[name]
	}

	if diff := cmp.Diff(want, stages); diff != "" {
		t.Errorf("task cpus/memory (-want +got):\n%s", diff)
	}
}

// resolvedLimits returns the cpus and memory the project's standard profile
// resolves process.resourceLimits to on this host, as `nextflow config -flat`
// and the trace both render them.
func resolvedLimits(t *testing.T, proj string) (string, string) {
	t.Helper()

	cmd := exec.Command("nextflow", "config", "-flat", "-profile", "standard")
	cmd.Dir = proj
	cmd.Env = append(os.Environ(), "NXF_ANSI_LOG=false", "NXF_OFFLINE=true")

	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("nextflow config: %v", err)
	}

	limits := flatLimits(string(out))

	if limits["cpus"] == "" || limits["memory"] == "" {
		t.Fatalf("nextflow config resolves no process.resourceLimits cpus/memory:\n%s", out)
	}

	return limits["cpus"], limits["memory"]
}

// TestFlatLimitsReadsBothForms: the clamp's limits read the same from
// Nextflow 26.04's per-key lines and 25.10's map value.
func TestFlatLimitsReadsBothForms(t *testing.T) {
	want := map[string]string{"cpus": "20", "memory": "15.3 GB"}

	for name, out := range map[string]string{
		"26.04": "process.resourceLimits.cpus = 20\nprocess.resourceLimits.memory = '15.3 GB'\n",
		"25.10": "process.executor = 'local'\nprocess.resourceLimits = [cpus:20, memory:'15.3 GB']\n",
	} {
		if d := cmp.Diff(want, flatLimits(out)); d != "" {
			t.Errorf("%s: limits (-want +got):\n%s", name, d)
		}
	}
}

// flatLimits reads process.resourceLimits from `nextflow config -flat` output,
// which Nextflow 26.04 renders one key per line and 25.10 as one map value:
// `process.resourceLimits = [cpus:20, memory:'15.3 GB']`.
func flatLimits(out string) map[string]string {
	limits := map[string]string{}

	for line := range strings.SplitSeq(out, "\n") {
		key, val, ok := strings.Cut(line, " = ")

		switch {
		case !ok:
		case strings.HasPrefix(key, "process.resourceLimits."):
			limits[strings.TrimPrefix(key, "process.resourceLimits.")] = strings.Trim(val, "'")
		case key == "process.resourceLimits":
			for entry := range strings.SplitSeq(strings.Trim(val, "[]"), ", ") {
				if k, v, ok := strings.Cut(entry, ":"); ok {
					limits[k] = strings.Trim(v, "'")
				}
			}
		}
	}

	return limits
}

// readTraceColumns returns the named columns of every row of a Nextflow trace.
func readTraceColumns(t *testing.T, path string, names ...string) [][]string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")

	cols := map[string]int{}
	for i, h := range strings.Split(lines[0], "\t") {
		cols[h] = i
	}

	rows := make([][]string, 0, len(lines)-1)

	for _, line := range lines[1:] {
		f := strings.Split(line, "\t")
		row := make([]string, len(names))

		for i, n := range names {
			c, ok := cols[n]
			if !ok {
				t.Fatalf("trace %s has no %q column", path, n)
			}

			row[i] = f[c]
		}

		rows = append(rows, row)
	}

	return rows
}
