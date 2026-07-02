//go:build e2e

package e2e

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

	metrics := filepath.Join(t.TempDir(), "metrics.jsonl")

	mf, err := os.Create(metrics)
	if err != nil {
		t.Fatal(err)
	}
	defer mf.Close()

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

		if w := os.Getenv("BENCH_WORKDIR"); w != "" {
			args = append(args, "-work-dir", w)
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

	report := exec.Command("python3", filepath.Join(root, "test", "e2e", "bench_report.py"),
		metrics, filepath.Join(root, "bench", "baseline.json"))

	out, err := report.CombinedOutput()

	t.Logf("bench report:\n%s", out)

	if err != nil {
		t.Fatalf("bench_report: %v", err)
	}
}
