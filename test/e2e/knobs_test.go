//go:build e2e

package e2e

import (
	"crypto/sha256"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/cg"
	"github.com/google/go-cmp/cmp"
)

// TestResumeCachesEverything: rerunning an unchanged, successful pipeline with
// -resume must execute ZERO new tasks (everything cached). A cache-key
// instability — e.g. a timestamp leaking into a staged asset, or a gather task
// whose input order is completion order — would otherwise ship silently and
// destroy resumability on long runs. The fixtures pin the gather shapes: a
// split triad's join of its chunk outs, a map call's gather of its forks, a
// split stage under a map call and a map call over a sub-pipeline holding
// a split; file_tree's join names its chunks' files by reference, which
// its reader stages from the chunks' work dirs (#562), and
// split_forward_file's split names its input by reference in each chunk's
// def, which each chunk and the join stage from GEN's work dir. Their calls are
// small, so each task writes the call into its
// script; staged_call's incompressible entry label pushes the call's
// encoding past cg.MaxValueBytes, so each task stages the call.b64 the driver wrote once
// (CALL), whose path every task's meta holds.
// These runtime cases are probabilistic guards: an unsorted gather only
// re-executes when completion order happens to churn.
func TestResumeCachesEverything(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java", "python3")

	for _, c := range []struct {
		name, fixture string
		params        map[string]any
	}{
		{"diamond_min", "diamond_min", nil},
		{"split_test", "split_test", nil},
		{"map_fork", "map_fork", nil},
		{"fork_min", "fork_min", nil},
		{"map_split", "map_split", nil},
		{"map_pipe_split", "map_pipe_split", nil},
		{"file_tree", "file_tree", nil},
		{"split_forward_file", "split_forward_file", nil},
		{"staged_call", "struct_input", map[string]any{"s": 10, "l": incompressible(2 * cg.MaxValueBytes), "x": 5}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			proj := transpile(t, c.fixture)

			var args []string
			if c.params != nil {
				writeParams(t, proj, c.params)
				args = []string{"-params-file", "params.json"}
			}

			if err := runNextflow(t, proj, slices.Concat(args, []string{"-with-trace", "trace1.txt"})...); err != nil {
				t.Fatalf("first run: %v", err)
			}

			assertCallStaged(t, filepath.Join(proj, "trace1.txt"), c.params != nil)

			if err := runNextflow(t, proj, slices.Concat(args, []string{"-resume", "-with-trace", "trace2.txt"})...); err != nil {
				t.Fatalf("-resume rerun: %v", err)
			}

			assertAllCached(t, readTrace(t, filepath.Join(proj, "trace2.txt")))
		})
	}
}

// incompressible is n characters of base64 of SHA-256 hashes, which gzip
// shrinks by no more than base64 grows them: a call holding it encodes to
// at least n bytes (cmd/mre encodeCall).
func incompressible(n int) string {
	var b strings.Builder

	for i := 0; b.Len() < n; i++ {
		sum := sha256.Sum256([]byte(strconv.Itoa(i)))
		b.WriteString(base64.RawURLEncoding.EncodeToString(sum[:]))
	}

	return b.String()[:n]
}

// assertCallStaged requires every task of the traced run that reads the
// run's call to have staged call.b64 when staged, or else written it from
// its script (callScript).
func assertCallStaged(t *testing.T, trace string, staged bool) {
	t.Helper()

	readers := 0

	for _, row := range readTraceColumns(t, trace, "name", "workdir") {
		script, err := os.ReadFile(filepath.Join(row[1], ".command.sh"))
		if err != nil {
			t.Fatalf("read %s's script: %v", row[0], err)
		}

		if !strings.Contains(string(script), "-call call.b64") {
			continue
		}

		readers++

		info, err := os.Lstat(filepath.Join(row[1], "call.b64"))
		if inline := strings.Contains(string(script), "<<'MRO2NF_CALL'"); inline == staged || err != nil || (info.Mode()&os.ModeSymlink != 0) != staged {
			t.Errorf("%s: call written by its script %t, call.b64 staged %t (%v), want staged %t", row[0], inline, err == nil && info.Mode()&os.ModeSymlink != 0, err, staged)
		}
	}

	if readers == 0 {
		t.Fatalf("no task in %s reads the run's call", trace)
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

// TestLocalPoolFloorsMemory pins pool() in stage.nf: a stage job is granted
// from the whole GB below process.resourceLimits' memory, as mrp's local
// job manager floors 90% of the host's memory to its pool, whether the
// config sets the memory as a MemoryUnit or a string.
func TestLocalPoolFloorsMemory(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java", "python3")

	for name, memory := range map[string]string{"unit": "4608.MB", "string": "'4.5 GB'"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			proj := transpile(t, "diamond_min")
			pool := writeOverlay(t, proj, "process.resourceLimits = [cpus: 2, memory: "+memory+"]\n")

			if err := runNextflow(t, proj, "-c", pool, "-with-trace", "pool-trace.txt"); err != nil {
				t.Fatal(err)
			}

			for _, row := range readTraceColumns(t, filepath.Join(proj, "pool-trace.txt"), "name", "workdir") {
				if row[0] != "N0_GEN (D.GEN)" {
					continue
				}

				script, err := os.ReadFile(filepath.Join(row[1], ".command.sh"))
				if err != nil {
					t.Fatalf("read GEN's script: %v", err)
				}

				if !strings.Contains(string(script), " -local-cores 2 -local-mem-gb 4 ") {
					t.Errorf("GEN's mre stage is not granted from 2 cpus and 4 GB:\n%s", script)
				}

				return
			}

			t.Fatal("trace has no N0_GEN task")
		})
	}
}

// TestLocalPoolWithoutLimitsFails: a user's own profile granting jobs as
// mrp's local job manager does, without process.resourceLimits, has no pool
// to grant from, so the run fails naming the setting instead of guessing one.
func TestLocalPoolWithoutLimitsFails(t *testing.T) {
	requireTools(t, "nextflow", "java", "python3")

	proj := transpile(t, "diamond_min")
	profile := writeOverlay(t, proj, "profiles { bare { process.executor = 'local' } }\n")

	err := runNextflowAllowingFailedAttempts(t, proj, "-c", profile, "-profile", "bare", "--mro2nf_local_jobs", "true")
	if err == nil {
		t.Fatal("run succeeded; local jobs without a pool must fail it")
	}

	log, err := os.ReadFile(filepath.Join(proj, "run.log"))
	if err != nil {
		t.Fatalf("read run.log: %v", err)
	}

	if !strings.Contains(string(log), "process.resourceLimits, which sets no cpus or memory") {
		t.Errorf("run.log does not name the missing pool:\n%s", log)
	}
}

// TestGridProfilesResolve: each grid profile a project advertises parses
// under `nextflow config`, selects its scheduler, reads head-node inputs in
// place, and grants jobs as mrp's cluster job managers do, with no local
// pool to grant from or clamp a task to. No CI runner has a scheduler, so
// this is the check the grid targets get.
func TestGridProfilesResolve(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java")

	proj := transpile(t, "split_test")

	for _, profile := range []string{"slurm", "sge", "lsf", "pbs"} {
		cmd := exec.Command("nextflow", "config", "-flat", "-profile", profile)
		cmd.Dir = proj
		cmd.Env = append(os.Environ(), "NXF_ANSI_LOG=false", "NXF_OFFLINE=true")

		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Errorf("nextflow config -profile %s: %v\n%s", profile, err, out)

			continue
		}

		for _, want := range []string{
			"process.executor = '" + profile + "'",
			"params.mro2nf_host_in_place = true",
			"params.mro2nf_local_jobs = false",
		} {
			if !strings.Contains(string(out), want) {
				t.Errorf("profile %s lacks %q:\n%s", profile, want, out)
			}
		}

		if strings.Contains(string(out), "process.resourceLimits") {
			t.Errorf("profile %s sets a local pool:\n%s", profile, out)
		}
	}
}

// writeOverlay writes body as a config in proj and returns its path.
func writeOverlay(t *testing.T, proj, body string) string {
	t.Helper()

	path := filepath.Join(proj, "overlay.config")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write overlay: %v", err)
	}

	return path
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

// TestFlatLimitsReadsPerKeyLines: the clamp's limits read from the lines
// `nextflow config -flat` renders process.resourceLimits as, one per key.
func TestFlatLimitsReadsPerKeyLines(t *testing.T) {
	want := map[string]string{"cpus": "20", "memory": "15.3 GB"}
	out := "process.executor = 'local'\nprocess.resourceLimits.cpus = 20\nprocess.resourceLimits.memory = '15.3 GB'\n"

	if d := cmp.Diff(want, flatLimits(out)); d != "" {
		t.Errorf("limits (-want +got):\n%s", d)
	}
}

// flatLimits reads process.resourceLimits from `nextflow config -flat` output.
func flatLimits(out string) map[string]string {
	limits := map[string]string{}

	for line := range strings.SplitSeq(out, "\n") {
		key, val, ok := strings.Cut(line, " = ")
		if ok && strings.HasPrefix(key, "process.resourceLimits.") {
			limits[strings.TrimPrefix(key, "process.resourceLimits.")] = strings.Trim(val, "'")
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
