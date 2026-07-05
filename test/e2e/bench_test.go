//go:build e2e

package e2e

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
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
	native    bool   // transpile with -native
}

// benchLanes returns every benchmark lane: each fixture invocation in default
// mode and again under -native (distinct `_native` baseline keys). The forks
// and split fixtures run at two widths (N=4 / N=16 forks, 4 / 16 chunks) so
// bench_report.py can assert the plumbing task count is IDENTICAL at both —
// the CLAUDE.md overhead rule that orchestration cost must not scale with
// fork width or chunk count. The forks×widths×native lanes are the ones that
// prove the -native O(1) element scatter stays O(1).
func benchLanes() []benchLane {
	base := []benchLane{
		{key: "chain", dir: "chain", mro: "pipeline.mro", probe: "MRE_BENCH_CHAIN_PROBE", producers: 1},
		{key: "forks_w4", dir: "forks", mro: "pipeline_w4.mro", probe: "MRE_BENCH_FORKS_PROBE", producers: 1},
		{key: "forks_w16", dir: "forks", mro: "pipeline_w16.mro", probe: "MRE_BENCH_FORKS_PROBE", producers: 1},
		{key: "split_c4", dir: "split", mro: "pipeline_c4.mro", probe: "MRE_BENCH_SPLIT_PROBE", producers: 1},
		{key: "split_c16", dir: "split", mro: "pipeline_c16.mro", probe: "MRE_BENCH_SPLIT_PROBE", producers: 1},
	}

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
// that plumbing_tasks is identical across a fixture's widths (BENCH_UPDATE=1
// records a new baseline; BENCH_PROFILE/BENCH_WORKDIR select another backend).
func TestBench(t *testing.T) {
	requireTools(t, "nextflow", "java", "python3")

	// A non-local BENCH_WORKDIR makes the refs gate vacuous (see
	// requireLocalWorkdir); validate once, up front, before any transpile or run.
	workdir := os.Getenv("BENCH_WORKDIR")
	if err := requireLocalWorkdir(workdir); err != nil {
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

	dir := filepath.Join(root, "bench", l.dir)
	if !fileExists(filepath.Join(dir, l.mro)) {
		t.Fatalf("bench fixture %s/%s missing", l.dir, l.mro)
	}

	var flags []string
	if l.native {
		flags = append(flags, "-native")
	}

	proj := transpileMRO(t, dir, l.mro, flags...)

	args := []string{"-with-trace", "trace.txt", "-with-dag", "dag.mmd"}
	if p := os.Getenv("BENCH_PROFILE"); p != "" {
		args = append(args, "-profile", p)
	}

	// scanRoot is where bench_metrics.py scans <scanRoot>/work for probe
	// stagings. Lanes run in parallel and a fixture's two widths share one
	// probe string, so a shared BENCH_WORKDIR gets a per-lane subdirectory —
	// otherwise one lane's stagings would inflate another's refs count.
	scanRoot := proj

	if workdir != "" {
		if _, path, ok := strings.Cut(workdir, "://"); ok {
			workdir = path // requireLocalWorkdir proved the scheme is file
		}

		scanRoot = filepath.Join(workdir, l.key)
		args = append(args, "-work-dir", filepath.Join(scanRoot, "work"))
	}

	if err := runNextflow(t, proj, args...); err != nil {
		t.Fatalf("bench %s: %v", l.key, err)
	}

	var out, errBuf bytes.Buffer

	cmd := exec.Command("python3", filepath.Join(root, "test", "e2e", "bench_metrics.py"),
		l.key, filepath.Join(proj, "trace.txt"), scanRoot, l.probe,
		strconv.Itoa(l.producers), filepath.Join(proj, "dag.mmd"))
	cmd.Stdout = &out
	cmd.Stderr = &errBuf

	if err := cmd.Run(); err != nil {
		t.Fatalf("bench_metrics %s: %v\n%s", l.key, err, errBuf.String())
	}

	return out.Bytes()
}

// requireLocalWorkdir rejects a non-local BENCH_WORKDIR (empty is fine — the
// default local executor). The bench gate's `refs` metric is a local scan of the
// work dir (bench_metrics.count_refs); over an object store (s3://…) that scan
// cannot walk the tree, so `refs` reads 0 and the regression gate would pass
// vacuously — a real S3-transfer regression would slip through. Rather than
// report a meaningless pass, the harness fails loudly: collecting the
// object-store metric is out of scope (see docs/BENCHMARKS.md). The scheme
// compare is case-insensitive (URI schemes are, per RFC 3986).
func requireLocalWorkdir(w string) error {
	if i := strings.Index(w, "://"); i >= 0 && !strings.EqualFold(w[:i], "file") {
		return fmt.Errorf("BENCH_WORKDIR=%q is non-local (%s://): the bench gate's "+
			"refs metric is a local work-dir scan and reads 0 over an object store, "+
			"so the gate would pass vacuously; run bench against a local work dir "+
			"(see docs/BENCHMARKS.md)", w, w[:i])
	}

	return nil
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
