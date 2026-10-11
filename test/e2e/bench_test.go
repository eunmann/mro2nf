//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eunmann/mro2nf/internal/emit"
	"github.com/eunmann/mro2nf/internal/frontend"
	"github.com/eunmann/mro2nf/internal/ir"
)

// benchLane is one benchmark run: a fixture invocation (dir + entry .mro)
// transpiled in either default or -native mode, measured under its own
// baseline key.
type benchLane struct {
	key       string // metrics name and bench/baseline.json key
	dir       string // fixture dir under bench/
	mro       string // entry invocation .mro within dir
	probe     string // marker string the fixture's probe file carries
	producers int    // how many tasks legitimately write the probe file
	// mrpJobs is the number of jobs real mrp runs for this invocation, which
	// the lane's stage tasks must equal (TestMrpDiffBenchJobs checks the
	// number against mrp itself).
	mrpJobs int
	// knownScaling is the open issue whose per-fork plumbing makes this
	// lane's plumbing grow with width; 0 when the lane must be width-flat.
	knownScaling int
	native       bool // transpile with -native
}

// issuePerForkPlumbing tracks the per-fork BIND_K/MERGE_K/DISABLE_K tasks a
// map over a sub-pipeline emits (#255).
const issuePerForkPlumbing = 255

// benchBaseLanes returns each fixture invocation once, in default mode. Every
// fixture but chain runs at two widths so bench_report.py can assert the
// plumbing task count is IDENTICAL at both — the CLAUDE.md overhead rule that
// orchestration cost must not scale with fork width or chunk count — or, for
// a lane marked knownScaling, that it has not got worse.
func benchBaseLanes() []benchLane {
	const known = issuePerForkPlumbing

	return []benchLane{
		{key: "chain", dir: "chain", mro: "pipeline.mro", probe: "MRE_BENCH_CHAIN_PROBE", producers: 1, mrpJobs: 5},
		{key: "forks_w4", dir: "forks", mro: "pipeline_w4.mro", probe: "MRE_BENCH_FORKS_PROBE", producers: 1, mrpJobs: 1 + 4},
		{key: "forks_w16", dir: "forks", mro: "pipeline_w16.mro", probe: "MRE_BENCH_FORKS_PROBE", producers: 1, mrpJobs: 1 + 16},
		{key: "split_c4", dir: "split", mro: "pipeline_c4.mro", probe: "MRE_BENCH_SPLIT_PROBE", producers: 1, mrpJobs: 4 + 2},
		{key: "split_c16", dir: "split", mro: "pipeline_c16.mro", probe: "MRE_BENCH_SPLIT_PROBE", producers: 1, mrpJobs: 16 + 2},
		// N samples, each a split of 2 chunks: split + 2 mains + join per sample.
		{key: "mapsplit_w4", dir: "mapsplit", mro: "pipeline_w4.mro", probe: "MRE_BENCH_MAPSPLIT_PROBE", producers: 4, mrpJobs: 4 * 4, knownScaling: known},
		{key: "mapsplit_w8", dir: "mapsplit", mro: "pipeline_w8.mro", probe: "MRE_BENCH_MAPSPLIT_PROBE", producers: 8, mrpJobs: 8 * 4, knownScaling: known},
		// The disabled USE runs no job, so mrp runs only each fork's MKFILE.
		{key: "mapdisabled_w4", dir: "mapdisabled", mro: "pipeline_w4.mro", probe: "MRE_BENCH_MAPDISABLED_PROBE", producers: 4, mrpJobs: 4, knownScaling: known},
		{key: "mapdisabled_w8", dir: "mapdisabled", mro: "pipeline_w8.mro", probe: "MRE_BENCH_MAPDISABLED_PROBE", producers: 8, mrpJobs: 8, knownScaling: known},
		// W samples x W lanes leaves, one CELL job each.
		{key: "mapnested_w2", dir: "mapnested", mro: "pipeline_w2.mro", probe: "MRE_BENCH_MAPNESTED_PROBE", producers: 2 * 2, mrpJobs: 2 * 2, knownScaling: known},
		{key: "mapnested_w4", dir: "mapnested", mro: "pipeline_w4.mro", probe: "MRE_BENCH_MAPNESTED_PROBE", producers: 4 * 4, mrpJobs: 4 * 4, knownScaling: known},
	}
}

// benchLanes returns every benchmark lane: each base lane in default mode and
// again under -native (distinct `_native` baseline keys). The forks×widths×
// native lanes are the ones that prove the -native O(1) element scatter stays
// O(1).
func benchLanes() []benchLane {
	base := benchBaseLanes()
	lanes := make([]benchLane, 0, 2*len(base))

	for _, l := range base {
		lanes = append(lanes, l)

		l.key += "_native"
		l.native = true
		lanes = append(lanes, l)
	}

	return lanes
}

// TestBench is the data-movement and orchestration-overhead benchmark gate
// (epic #18 / #17, overhead rule #119): each bench lane runs under Nextflow
// with tracing, bench_metrics.py measures the probe-file staging count and
// the task/plumbing shape, and bench_report.py gates refs, plumbing_tasks,
// and tasks against bench/baseline.json — plus the width-scaling invariant
// that plumbing_tasks is identical across a fixture's widths, and that each
// lane's stage tasks equal mrp's job count (BENCH_UPDATE=1 records a new
// baseline; BENCH_PROFILE/BENCH_WORKDIR select another backend).
func TestBench(t *testing.T) {
	requireTools(t, "nextflow", "java", "python3")

	// A non-local BENCH_WORKDIR makes the refs gate vacuous (see
	// requireLocalWorkdir); validate once, up front, before any transpile or run.
	workdir := os.Getenv("BENCH_WORKDIR")
	if err := requireLocalWorkdir(workdir); err != nil {
		t.Fatal(err)
	}

	// Absolutize once at the harness boundary: nextflow resolves a relative
	// -work-dir against the project dir while bench_metrics.py scans relative
	// to the test cwd, so a relative BENCH_WORKDIR would split the two roots
	// and the refs scan would read 0 — a vacuous pass.
	workdir, err := absLocalWorkdir(workdir)
	if err != nil {
		t.Fatal(err)
	}

	var (
		mu      sync.Mutex
		metrics bytes.Buffer
	)

	ok := t.Run("lanes", func(t *testing.T) {
		for _, l := range benchLanes() {
			t.Run(l.key, func(t *testing.T) {
				t.Parallel()

				line := runBenchLane(t, l, workdir)

				mu.Lock()
				defer mu.Unlock()

				metrics.Write(line)
			})
		}
	})
	if !ok {
		t.Fatal("bench lanes failed; skipping the report gate")
	}

	path := filepath.Join(t.TempDir(), "metrics.jsonl")
	if err := os.WriteFile(path, metrics.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	runBenchReport(t, path)
}

// runBenchLane transpiles, runs, and measures one lane, returning its
// bench_metrics.py JSON line.
func runBenchLane(t *testing.T, l benchLane, workdir string) []byte {
	t.Helper()

	line, _ := runLane(t, l, laneRun{dir: filepath.Join(root, "bench", l.dir), workdir: workdir})

	return line
}

// laneRun is where and how runLane runs a bench lane.
type laneRun struct {
	dir     string // fixture dir holding the lane's mro; the scale tier generates one
	workdir string // BENCH_WORKDIR root, or "" for the project's own work/
	mre     string // the mre the project calls, or "" for the repo build
	jvm     string // appended to the driver's NXF_OPTS
	gcLogs  string // dir the jvm options write GC logs to, or ""
}

// runLane transpiles, runs, and measures one lane as r says, returning its
// bench_metrics.py JSON line and the project dir.
func runLane(t *testing.T, l benchLane, r laneRun) ([]byte, string) {
	t.Helper()

	if !fileExists(filepath.Join(r.dir, l.mro)) {
		t.Fatalf("bench fixture %s/%s missing", l.dir, l.mro)
	}

	proj := transpileLane(t, l, r)

	args := []string{"-with-trace", "trace.txt", "-with-dag", "dag.mmd"}
	if p := os.Getenv("BENCH_PROFILE"); p != "" {
		args = append(args, "-profile", p)
	}

	// scanRoot is where bench_metrics.py scans <scanRoot>/work for probe
	// stagings. Lanes run in parallel and a fixture's two widths share one
	// probe string, so a shared BENCH_WORKDIR gets a per-lane subdirectory —
	// otherwise one lane's stagings would inflate another's refs count.
	scanRoot := proj

	if r.workdir != "" {
		scanRoot = filepath.Join(r.workdir, l.key)
		args = append(args, "-work-dir", filepath.Join(scanRoot, "work"))
	}

	start := time.Now()

	if err := runNextflowJVM(t, proj, r.jvm, args...); err != nil {
		t.Fatalf("bench %s: %v", l.key, err)
	}

	wall := time.Since(start)

	stages, err := writeStageProcesses(r.dir, l.mro, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	var out, errBuf bytes.Buffer

	cmd := exec.Command("python3", filepath.Join(root, "test", "e2e", "bench_metrics.py"),
		"--name", l.key, "--trace", filepath.Join(proj, "trace.txt"), "--workdir", scanRoot,
		"--probe", l.probe, "--producers", strconv.Itoa(l.producers),
		"--stages", stages, "--dag", filepath.Join(proj, "dag.mmd"),
		"--mrp-jobs", strconv.Itoa(l.mrpJobs), "--known-scaling", strconv.Itoa(l.knownScaling),
		"--gc-logs", r.gcLogs, "--wall-seconds", strconv.FormatFloat(wall.Seconds(), 'f', 1, 64))
	cmd.Stdout = &out
	cmd.Stderr = &errBuf

	if err := cmd.Run(); err != nil {
		t.Fatalf("bench_metrics %s: %v\n%s", l.key, err, errBuf.String())
	}

	return out.Bytes(), proj
}

// transpileLane transpiles the lane's mro in r.dir, with -native for a native
// lane and r.mre as the mre the project calls.
func transpileLane(t *testing.T, l benchLane, r laneRun) string {
	t.Helper()

	var flags []string
	if l.native {
		flags = append(flags, "-native")
	}

	if r.mre == "" {
		return transpileMRO(t, r.dir, l.mro, flags...)
	}

	buildBinaries(t)

	tools := repoTools()
	tools.mre = r.mre

	proj := t.TempDir()
	if err := transpileInto(tools, r.dir, l.mro, proj, flags...); err != nil {
		t.Fatalf("transpile %s: %v", l.key, err)
	}

	return proj
}

// writeStageProcesses writes, as a JSON array under tmp, every process name
// the emitter can give a task that runs a stage's own code in the program of
// dir/mro, and returns the file's path. stageProcessNames builds them from
// the same inventory as the override selectors. bench_metrics.py counts
// every other task as plumbing, so a new helper process is plumbing without
// being listed.
func writeStageProcesses(dir, mro, tmp string) (string, error) {
	prog, err := lowerMRO(dir, mro)
	if err != nil {
		return "", err
	}

	data, err := json.Marshal(stageProcessNames(prog))
	if err != nil {
		return "", fmt.Errorf("encode stage processes: %w", err)
	}

	path := filepath.Join(tmp, "stage_processes.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("write stage processes: %w", err)
	}

	return path, nil
}

// lowerMRO parses and lowers the program of dir/mro.
func lowerMRO(dir, mro string) (*ir.Program, error) {
	mroPath := []string{dir}

	ast, err := frontend.Parse(filepath.Join(dir, mro), mroPath, false)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", mro, err)
	}

	prog, err := frontend.Lower(ast, mroPath)
	if err != nil {
		return nil, fmt.Errorf("lower %s: %w", mro, err)
	}

	return prog, nil
}

// stageProcessNames returns, sorted, every process name the emitter can give a
// task that runs a stage's own code: per stage the plain family, per pipeline
// call the fused family and the keyed element scatter. Built from the exported
// suffix inventory and emit.Qualify, it is the list writeStageProcesses counts as stage tasks;
// internal/emit's TestStageProcessNames pins the same loop against the
// processes the emitter actually generates.
func stageProcessNames(prog *ir.Program) []string {
	var names []string

	for stage := range prog.Stages {
		for _, s := range emit.PlainStageSuffixes() {
			names = append(names, stage+s)
		}
	}

	for _, p := range prog.Pipelines {
		for _, c := range p.Calls {
			q := emit.Qualify(p.Name, c.Name)

			for _, s := range emit.FusedCallSuffixes() {
				names = append(names, "STAGE_"+q+s)
			}

			for _, s := range emit.ScatterCallSuffixes() {
				names = append(names, "FORK_"+q+s)
			}
		}
	}

	slices.Sort(names)

	return names
}

// TestMrpDiffBenchJobs runs every bench invocation under real mrp and
// requires its job count to equal the lane's mrpJobs, the number TestBench
// holds the lane's stage tasks to. mrp writes one _jobinfo per job (each
// split, chunk and join of a split stage; one per non-split stage fork; none
// for a disabled call).
func TestMrpDiffBenchJobs(t *testing.T) {
	t.Parallel()

	bin := martianBin(t)

	for _, l := range benchBaseLanes() {
		t.Run(l.key, func(t *testing.T) {
			t.Parallel()
			shardSkip(t)

			tmp := t.TempDir()
			runMrpAt(t, bin, filepath.Join(root, "bench", l.dir), l.mro, tmp)

			jobs := 0

			err := filepath.WalkDir(filepath.Join(tmp, "mrp"), func(_ string, d fs.DirEntry, err error) error {
				if err == nil && !d.IsDir() && d.Name() == "_jobinfo" {
					jobs++
				}

				return err
			})
			if err != nil {
				t.Fatalf("walk pipestance: %v", err)
			}

			if jobs != l.mrpJobs {
				t.Errorf("mrp ran %d jobs for %s/%s, lane %s declares mrpJobs %d", jobs, l.dir, l.mro, l.key, l.mrpJobs)
			}
		})
	}
}

// errNonLocalWorkdir rejects a BENCH_WORKDIR on an object store, where the
// refs metric cannot be collected (see requireLocalWorkdir).
var errNonLocalWorkdir = errors.New("BENCH_WORKDIR is non-local")

// requireLocalWorkdir rejects a non-local BENCH_WORKDIR (empty is fine — the
// default local executor). The bench gate's `refs` metric is a local scan of the
// work dir (bench_metrics.count_refs); over an object store (s3://…) that scan
// cannot walk the tree, so `refs` reads 0 and the regression gate would pass
// vacuously — a real S3-transfer regression would slip through. Rather than
// report a meaningless pass, the harness fails loudly: collecting the
// object-store metric is out of scope (see docs/BENCHMARKS.md). The scheme
// compare is case-insensitive (URI schemes are, per RFC 3986).
func requireLocalWorkdir(w string) error {
	if before, _, ok := strings.Cut(w, "://"); ok && !strings.EqualFold(before, "file") {
		return fmt.Errorf("%w: BENCH_WORKDIR=%q (%s://): the bench gate's "+
			"refs metric is a local work-dir scan and reads 0 over an object store, "+
			"so the gate would pass vacuously; run bench against a local work dir "+
			"(see docs/BENCHMARKS.md)", errNonLocalWorkdir, w, before)
	}

	return nil
}

// absLocalWorkdir normalizes a local BENCH_WORKDIR (already vetted by
// requireLocalWorkdir) to one absolute path: it strips a file:// scheme and
// resolves a relative path against the current working directory. Both
// consumers of the value — nextflow's -work-dir (cwd = project dir) and
// bench_metrics.py's refs scan (cwd = test dir) — must receive the SAME
// absolute root; handing them a relative path would silently split them and
// the refs gate would count 0.
func absLocalWorkdir(w string) (string, error) {
	if w == "" {
		return "", nil
	}

	if _, path, ok := strings.Cut(w, "://"); ok {
		w = path // requireLocalWorkdir proved the scheme is file
	}

	abs, err := filepath.Abs(w)
	if err != nil {
		return "", fmt.Errorf("absolutize BENCH_WORKDIR %q: %w", w, err)
	}

	return abs, nil
}

func TestRequireLocalWorkdir(t *testing.T) {
	cases := []struct {
		workdir string
		wantErr bool
	}{
		{"", false},
		{"/tmp/work", false},
		{"./work", false},
		{"file:///tmp/work", false},
		{"FILE:///tmp/work", false}, // schemes are case-insensitive
		{"s3://bucket/work", true},
		{"gs://bucket/work", true},
		{"az://container/work", true},
	}

	for _, c := range cases {
		err := requireLocalWorkdir(c.workdir)
		if (err != nil) != c.wantErr {
			t.Errorf("requireLocalWorkdir(%q) error = %v, wantErr %v", c.workdir, err, c.wantErr)
		}
	}
}

// TestAbsLocalWorkdir proves a relative BENCH_WORKDIR is resolved to ONE
// consistent absolute root before it feeds both nextflow's -work-dir and
// bench_metrics.py's scan — the two run from different cwds, so a relative
// value passed through unchanged would scan the wrong tree (refs=0, vacuous
// pass).
func TestAbsLocalWorkdir(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		workdir string
		want    string
	}{
		{"", ""}, // empty stays empty: default local executor, scanRoot = proj
		{"work", filepath.Join(cwd, "work")},
		{"./work", filepath.Join(cwd, "work")},
		{"/tmp/bench-work", "/tmp/bench-work"},
		{"file:///tmp/bench-work", "/tmp/bench-work"},
		{"file://rel/work", filepath.Join(cwd, "rel", "work")},
	}

	for _, c := range cases {
		got, err := absLocalWorkdir(c.workdir)
		if err != nil {
			t.Errorf("absLocalWorkdir(%q) error = %v", c.workdir, err)

			continue
		}

		if got != c.want {
			t.Errorf("absLocalWorkdir(%q) = %q, want %q", c.workdir, got, c.want)
		}

		if got != "" && !filepath.IsAbs(got) {
			t.Errorf("absLocalWorkdir(%q) = %q is not absolute", c.workdir, got)
		}
	}
}

func runBenchReport(t *testing.T, metrics string) {
	t.Helper()

	report := exec.Command("python3", filepath.Join(root, "test", "e2e", "bench_report.py"),
		metrics, filepath.Join(root, "bench", "baseline.json"))

	out, err := report.CombinedOutput()

	t.Logf("bench report:\n%s", out)

	if err != nil {
		t.Fatalf("bench_report: %v", err)
	}
}
