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
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// benchLane is one benchmark run: a fixture invocation (dir + entry .mro),
// measured under its own baseline key.
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
	// knownScaling is the open issue whose plumbing makes this lane's
	// plumbing grow with width; 0 when the lane must be width-flat.
	knownScaling int
	// flatStaging holds the most bytes one stage task stages to the same
	// at every width (bench_report.py check_staging): each fork stages its
	// own share, never one that grows with the fork width.
	flatStaging bool
}

// benchLanes returns each fixture invocation once. Every fixture but chain
// runs at two widths so bench_report.py can assert the plumbing task count is
// IDENTICAL at both — the CLAUDE.md overhead rule that orchestration cost
// must not scale with fork width or chunk count — or, for a lane marked
// knownScaling, that it has not got worse.
func benchLanes() []benchLane {
	return []benchLane{
		{key: "chain", dir: "chain", mro: "pipeline.mro", probe: "MRE_BENCH_CHAIN_PROBE", producers: 1, mrpJobs: 5},
		{key: "forks_w4", dir: "forks", mro: "pipeline_w4.mro", probe: "MRE_BENCH_FORKS_PROBE", producers: 1, mrpJobs: 1 + 4},
		{key: "forks_w16", dir: "forks", mro: "pipeline_w16.mro", probe: "MRE_BENCH_FORKS_PROBE", producers: 1, mrpJobs: 1 + 16},
		// MKPAY, then WSPLIT's split, N chunks and join.
		{key: "split_c4", dir: "split", mro: "pipeline_c4.mro", probe: "MRE_BENCH_SPLIT_PROBE", producers: 1, mrpJobs: 1 + 4 + 2},
		{key: "split_c16", dir: "split", mro: "pipeline_c16.mro", probe: "MRE_BENCH_SPLIT_PROBE", producers: 1, mrpJobs: 1 + 16 + 2},
		// N samples, each a split of 2 chunks: split + 2 mains + join per sample.
		{key: "mapsplit_w4", dir: "mapsplit", mro: "pipeline_w4.mro", probe: "MRE_BENCH_MAPSPLIT_PROBE", producers: 4, mrpJobs: 4 * 4},
		{key: "mapsplit_w8", dir: "mapsplit", mro: "pipeline_w8.mro", probe: "MRE_BENCH_MAPSPLIT_PROBE", producers: 8, mrpJobs: 8 * 4},
		// The disabled USE runs no job, so mrp runs only each fork's MKFILE.
		{key: "mapdisabled_w4", dir: "mapdisabled", mro: "pipeline_w4.mro", probe: "MRE_BENCH_MAPDISABLED_PROBE", producers: 4, mrpJobs: 4},
		{key: "mapdisabled_w8", dir: "mapdisabled", mro: "pipeline_w8.mro", probe: "MRE_BENCH_MAPDISABLED_PROBE", producers: 8, mrpJobs: 8},
		// FLAGS, then in each of STATIC and GATED a MKFILE per fork and a USE
		// at each fork its skip holds enabled, half of them. Each of GATED's
		// USE tasks stages only its own element of FLAGS.skips, which its
		// disable binding reads.
		{key: "forkdisabled_w4", dir: "forkdisabled", mro: "pipeline_w4.mro", probe: "MRE_BENCH_FORKDISABLED_PROBE", producers: 2 * 4, mrpJobs: 1 + 2*(4+4/2), flatStaging: true},
		{key: "forkdisabled_w16", dir: "forkdisabled", mro: "pipeline_w16.mro", probe: "MRE_BENCH_FORKDISABLED_PROBE", producers: 2 * 16, mrpJobs: 1 + 2*(16+16/2), flatStaging: true},
		// W samples x W lanes leaves, one CELL job each.
		{key: "mapnested_w2", dir: "mapnested", mro: "pipeline_w2.mro", probe: "MRE_BENCH_MAPNESTED_PROBE", producers: 2 * 2, mrpJobs: 2 * 2},
		{key: "mapnested_w4", dir: "mapnested", mro: "pipeline_w4.mro", probe: "MRE_BENCH_MAPNESTED_PROBE", producers: 4 * 4, mrpJobs: 4 * 4},
		// MKVALS and MKFILES, then one USE per fork zipping their outputs.
		{key: "zip_w4", dir: "zip", mro: "pipeline_w4.mro", probe: "MRE_BENCH_ZIP_PROBE", producers: 1, mrpJobs: 2 + 4, flatStaging: true},
		{key: "zip_w16", dir: "zip", mro: "pipeline_w16.mro", probe: "MRE_BENCH_ZIP_PROBE", producers: 1, mrpJobs: 2 + 16, flatStaging: true},
		// MKVALS, GATE and the MKFILES it gates, then one USE per fork zipping
		// MKVALS's values with MKFILES's files through the gate (#494).
		{key: "zipgated_w4", dir: "zipgated", mro: "pipeline_w4.mro", probe: "MRE_BENCH_ZIPGATED_PROBE", producers: 1, mrpJobs: 3 + 4, flatStaging: true},
		{key: "zipgated_w16", dir: "zipgated", mro: "pipeline_w16.mro", probe: "MRE_BENCH_ZIPGATED_PROBE", producers: 1, mrpJobs: 3 + 16, flatStaging: true},
		// MKXS and GATE, then in each of 4 lanes an A per fork writing one
		// file and a B per fork reading the A file at its own index (#601):
		// each B task stages that one file.
		{key: "mapchain_w4", dir: "mapchain", mro: "pipeline_w4.mro", probe: "MRE_BENCH_MAPCHAIN_PROBE", producers: 4 * 4, mrpJobs: 2 + 8*4, flatStaging: true},
		{key: "mapchain_w16", dir: "mapchain", mro: "pipeline_w16.mro", probe: "MRE_BENCH_MAPCHAIN_PROBE", producers: 4 * 16, mrpJobs: 2 + 8*16, flatStaging: true},
		// MKXS, then in each of 2 lanes an A per fork writing one file and
		// a B per fork reading the A file at its own index and every A
		// file (#622): each B task stages all N A files, as each mrp job
		// reads them, so its staging grows with the width.
		{key: "mapgather_w4", dir: "mapgather", mro: "pipeline_w4.mro", probe: "MRE_BENCH_MAPGATHER_PROBE", producers: 2 * 4, mrpJobs: 1 + 4*4},
		{key: "mapgather_w16", dir: "mapgather", mro: "pipeline_w16.mro", probe: "MRE_BENCH_MAPGATHER_PROBE", producers: 2 * 16, mrpJobs: 1 + 4*16},
		// MKREF, then per sample ALIGN's split, 2 chunks and join, whose
		// chunks and join read MKREF's ref.
		{key: "splitbroadcast_w4", dir: "splitbroadcast", mro: "pipeline_w4.mro", probe: "MRE_BENCH_SPLITBROADCAST_PROBE", producers: 1, mrpJobs: 1 + 4*4, flatStaging: true},
		{key: "splitbroadcast_w8", dir: "splitbroadcast", mro: "pipeline_w8.mro", probe: "MRE_BENCH_SPLITBROADCAST_PROBE", producers: 1, mrpJobs: 1 + 8*4, flatStaging: true},
	}
}

// TestBench is the data-movement and orchestration-overhead benchmark gate
// (epic #18 / #17, overhead rule #119): each bench lane runs under Nextflow
// with tracing, bench_metrics.py measures the probe-file staging count and
// the task/plumbing shape, and bench_report.py gates refs, plumbing_tasks,
// and tasks against bench/baseline.json — plus the width-scaling invariants
// that plumbing_tasks, and restaged_bytes per producer, are identical across
// a fixture's widths, and that each lane's stage tasks equal mrp's job
// count (BENCH_UPDATE=1 records a new
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

	stages, err := writeStageProcesses(proj, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	var out, errBuf bytes.Buffer

	metricArgs := []string{
		filepath.Join(root, "test", "e2e", "bench_metrics.py"),
		"--name", l.key, "--trace", filepath.Join(proj, "trace.txt"), "--workdir", scanRoot,
		"--probe", l.probe, "--producers", strconv.Itoa(l.producers),
		"--stages", stages, "--dag", filepath.Join(proj, "dag.mmd"),
		"--mrp-jobs", strconv.Itoa(l.mrpJobs), "--known-scaling", strconv.Itoa(l.knownScaling),
		"--gc-logs", r.gcLogs, "--wall-seconds", strconv.FormatFloat(wall.Seconds(), 'f', 1, 64),
	}
	if l.flatStaging {
		metricArgs = append(metricArgs, "--flat-staging")
	}

	cmd := exec.Command("python3", metricArgs...)
	cmd.Stdout = &out
	cmd.Stderr = &errBuf

	if err := cmd.Run(); err != nil {
		t.Fatalf("bench_metrics %s: %v\n%s", l.key, err, errBuf.String())
	}

	return out.Bytes(), proj
}

// transpileLane transpiles the lane's mro in r.dir, with r.mre as the mre the
// project calls.
func transpileLane(t *testing.T, l benchLane, r laneRun) string {
	t.Helper()

	if r.mre == "" {
		return transpileMRO(t, r.dir, l.mro)
	}

	buildBinaries(t)

	tools := repoTools()
	tools.mre = r.mre

	proj := t.TempDir()
	if err := transpileInto(tools, r.dir, l.mro, proj); err != nil {
		t.Fatalf("transpile %s: %v", l.key, err)
	}

	return proj
}

// writeStageProcesses writes, as a JSON array under tmp, every process name
// that runs a stage's own code in proj, the stage module's aliases its
// main.nf and sub-workflow modules include, and returns the file's path. bench_metrics.py counts
// every other task as plumbing, so a new helper process is plumbing without
// being listed.
func writeStageProcesses(proj, tmp string) (string, error) {
	names, err := stageAliases(proj)
	if err != nil {
		return "", err
	}

	data, err := json.Marshal(names)
	if err != nil {
		return "", fmt.Errorf("encode stage processes: %w", err)
	}

	path := filepath.Join(tmp, "stage_processes.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("write stage processes: %w", err)
	}

	return path, nil
}

// stageInclude matches one alias of the stage module a call-graph project's
// main.nf or sub-workflow module includes: `STAGE as N0_X`, `STAGE_SPLIT as
// N1_Y_SP`, ...
var stageInclude = regexp.MustCompile(`\bSTAGE(?:_SPLIT|_MAIN|_JOIN)? as (\w+)`)

// stageAliases are the process aliases of the stage module in proj's
// main.nf and sub-workflow modules, sorted.
func stageAliases(proj string) ([]string, error) {
	modules, err := filepath.Glob(filepath.Join(proj, "subworkflows", "*.nf"))
	if err != nil {
		return nil, fmt.Errorf("list sub-workflow modules: %w", err)
	}

	var names []string

	for _, path := range append([]string{filepath.Join(proj, "main.nf")}, modules...) {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}

		for _, m := range stageInclude.FindAllStringSubmatch(string(raw), -1) {
			names = append(names, m[1])
		}
	}

	slices.Sort(names)

	return names, nil
}

// TestMrpDiffBenchJobs runs every bench invocation under real mrp and
// requires its job count to equal the lane's mrpJobs, the number TestBench
// holds the lane's stage tasks to. mrp writes one _jobinfo per job (each
// split, chunk and join of a split stage; one per non-split stage fork; none
// for a disabled call).
func TestMrpDiffBenchJobs(t *testing.T) {
	t.Parallel()

	bin := martianBin(t)

	for _, l := range benchLanes() {
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
// object-store metric is out of scope (see docs/development/benchmarks.md).
// The scheme compare is case-insensitive (URI schemes are, per RFC 3986).
func requireLocalWorkdir(w string) error {
	if before, _, ok := strings.Cut(w, "://"); ok && !strings.EqualFold(before, "file") {
		return fmt.Errorf("%w: BENCH_WORKDIR=%q (%s://): the bench gate's "+
			"refs metric is a local work-dir scan and reads 0 over an object store, "+
			"so the gate would pass vacuously; run bench against a local work dir "+
			"(see docs/development/benchmarks.md)", errNonLocalWorkdir, w, before)
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
