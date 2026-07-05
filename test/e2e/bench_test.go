//go:build e2e

package e2e

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestBench is the data-movement benchmark gate (epic #18 / #17): each bench
// pipeline runs under Nextflow with tracing, bench_metrics.py measures how
// often the probe file is staged across the work dir, and bench_report.py
// gates the transfer multiplier against bench/baseline.json (BENCH_UPDATE=1
// records a new baseline; BENCH_PROFILE/BENCH_WORKDIR select another backend).
// Port of bench.sh; the Python metric scripts are retained as-is.
func TestBench(t *testing.T) {
	requireTools(t, "nextflow", "java", "python3")

	benches := []struct {
		name      string
		probe     string
		producers int
	}{
		{"chain", "MRE_BENCH_CHAIN_PROBE", 1},
		{"split", "MRE_BENCH_SPLIT_PROBE", 1},
	}

	// A non-local BENCH_WORKDIR makes the refs gate vacuous (see
	// requireLocalWorkdir); validate once, up front, before any transpile or run.
	workdir := os.Getenv("BENCH_WORKDIR")
	if err := requireLocalWorkdir(workdir); err != nil {
		t.Fatal(err)
	}

	metrics := filepath.Join(t.TempDir(), "metrics.jsonl")

	mf, err := os.Create(metrics)
	if err != nil {
		t.Fatal(err)
	}
	// The checked Close below runs before the report; this deferred one only
	// covers the t.Fatalf exits inside the loop, where a leaked fd is moot.
	defer func() { _ = mf.Close() }()

	for _, b := range benches {
		dir := filepath.Join(root, "bench", b.name)
		if !fileExists(filepath.Join(dir, "pipeline.mro")) {
			t.Fatalf("bench fixture %s missing", b.name)
		}

		proj := transpileDir(t, dir)

		args := []string{"-with-trace", "trace.txt", "-with-dag", "dag.mmd"}
		if p := os.Getenv("BENCH_PROFILE"); p != "" {
			args = append(args, "-profile", p)
		}

		if workdir != "" {
			args = append(args, "-work-dir", workdir)
		}

		if err := runNextflow(t, proj, args...); err != nil {
			t.Fatalf("bench %s: %v", b.name, err)
		}

		var errBuf bytes.Buffer

		cmd := exec.Command("python3", filepath.Join(root, "test", "e2e", "bench_metrics.py"),
			b.name, filepath.Join(proj, "trace.txt"), proj, b.probe,
			strconv.Itoa(b.producers), filepath.Join(proj, "dag.mmd"))
		cmd.Stdout = mf
		cmd.Stderr = &errBuf

		if err := cmd.Run(); err != nil {
			t.Fatalf("bench_metrics %s: %v\n%s", b.name, err, errBuf.String())
		}
	}

	// bench_metrics.py wrote through mf; a close error means the metrics file
	// may be incomplete, so fail before gating on it.
	if err := mf.Close(); err != nil {
		t.Fatalf("close %s: %v", metrics, err)
	}

	runBenchReport(t, metrics)
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
