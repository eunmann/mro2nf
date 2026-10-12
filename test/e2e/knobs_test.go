//go:build e2e

package e2e

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestResumeCachesEverything: rerunning an unchanged, successful pipeline with
// -resume must execute ZERO new tasks (everything cached). A cache-key
// instability — e.g. a timestamp leaking into a staged asset, or a gather task
// whose input order is completion order — would otherwise ship silently and
// destroy resumability on long runs. The table pins the gather shapes that
// carry order countermeasures (#123): the -native element scatter, the keyed
// split triad's JOIN_K, the keyed nested-map MERGE_K, the non-keyed split
// triad's JOIN (plain + fused), and the default-mode mapped call's MERGE.
// These runtime cases are probabilistic guards —
// an unsorted gather only re-executes when completion order happens to churn;
// the emit-side text pins are the deterministic ones. (Port of the -resume
// half of runtime_knobs.sh.)
func TestResumeCachesEverything(t *testing.T) {
	t.Parallel()

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
		// Default-mode mapped call: FORK fan-out whose MERGE gathers the
		// per-fork outs sorted by name (off -native, MERGE never folds away).
		{"map_fork", "map_fork", nil},
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

			if slices.Contains(tc.flags, "-native") {
				skipUnderV2(t, "-native")
			}

			proj := transpile(t, tc.fixture, tc.flags...)

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

// TestOverridesBeatChunkRequest pins #248 against real mrp: under
// `--overrides {"FRAC_MEM.CHUNKED": {"chunk.mem_gb": 3, "join.mem_gb": 4}}` mrp
// gives BOTH of CHUNKED's chunks 3 GB in _jobinfo and __mem_gb (and __vmem_gb
// 6), although its split asked 0.5 GB for chunk 0, and the join 4 GB over the
// split-returned 0.5: mrp's getJobReqs applies the override after the chunk's
// and join's own request. The untargeted MEM stage keeps its 1.5.
func TestOverridesBeatChunkRequest(t *testing.T) {
	requireTools(t, "nextflow", "java", "python3")
	buildBinaries(t)

	work := t.TempDir()
	overridesJSON := filepath.Join(work, "overrides.json")

	if err := os.WriteFile(overridesJSON, []byte(`{ "FRAC_MEM.CHUNKED": { "chunk.mem_gb": 3, "join.mem_gb": 4 } }`), 0o644); err != nil {
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

	proj := transpile(t, "frac_mem")

	if err := runNextflow(t, proj, "-c", overlay); err != nil {
		t.Fatal(err)
	}

	var outs struct {
		ChunkM  []float64 `json:"chunk_m"`
		ChunkAM []float64 `json:"chunk_am"`
		JoinM   float64   `json:"join_m"`
		JoinAM  float64   `json:"join_am"`
		MemM    float64   `json:"mem_m"`
	}

	readJSON(t, filepath.Join(proj, "results", "pipeline_outs.json"), &outs)

	want := []float64{3, 3}
	if fmt.Sprint(outs.ChunkM) != fmt.Sprint(want) || fmt.Sprint(outs.ChunkAM) != fmt.Sprint(want) {
		t.Errorf("chunks saw mem %v / __mem_gb %v, want %v (mrp: the chunk.mem_gb override beats the chunk's own request)",
			outs.ChunkM, outs.ChunkAM, want)
	}

	if outs.JoinM != 4 || outs.JoinAM != 4 {
		t.Errorf("join saw mem %v / __mem_gb %v, want 4 (mrp: the join.mem_gb override beats the split's join request)",
			outs.JoinM, outs.JoinAM)
	}

	if outs.MemM != 1.5 {
		t.Errorf("untargeted MEM saw %v, want its own 1.5", outs.MemM)
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

// TestLocalClampRunsOversizedRequest pins #321: on the standard profile a stage
// requesting more cpus and memory than the host runs with the host's
// process.resourceLimits, where Nextflow's local executor would otherwise fail
// it ("Process requirement exceeds available CPUs"). The untargeted ADD keeps
// its own request.
func TestLocalClampRunsOversizedRequest(t *testing.T) {
	requireTools(t, "nextflow", "java", "python3")

	proj := transpile(t, "diamond_min")

	overlay := filepath.Join(proj, "oversized.config")
	body := "process { withName: 'STAGE_1_D__GEN' { cpus = 4096; memory = '100000 GB' } }\n" +
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
		"D:STAGE_1_D__GEN": {wantCpus, wantMem},
		"D:STAGE_1_D__ADD": {"1", "1 GB"},
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

	limits := map[string]string{}

	for line := range strings.SplitSeq(string(out), "\n") {
		key, val, ok := strings.Cut(line, " = ")
		if ok && strings.HasPrefix(key, "process.resourceLimits.") {
			limits[strings.TrimPrefix(key, "process.resourceLimits.")] = strings.Trim(val, "'")
		}
	}

	if limits["cpus"] == "" || limits["memory"] == "" {
		t.Fatalf("nextflow config resolves no process.resourceLimits cpus/memory:\n%s", out)
	}

	return limits["cpus"], limits["memory"]
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

// stageJobinfos walks the work tree and returns the _jobinfo the GEN and ADD
// stages saw. _jobinfo's own `name` carries the entry callable, so the stage
// is identified from the task dir's .command.run header instead.
func stageJobinfos(t *testing.T, work string) (*jobinfo, *jobinfo) {
	t.Helper()

	var gen, add *jobinfo

	walkErr := filepath.WalkDir(work, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "_jobinfo" {
			return err
		}

		// work/<xx>/<hash>/main/_jobinfo -> the task dir holds .command.run.
		runFile := filepath.Join(filepath.Dir(filepath.Dir(path)), ".command.run")

		raw, readErr := os.ReadFile(runFile)
		if errors.Is(readErr, fs.ErrNotExist) {
			return nil // no .command.run two levels up: not a task's _jobinfo
		}

		if readErr != nil {
			return fmt.Errorf("read %s: %w", runFile, readErr)
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
