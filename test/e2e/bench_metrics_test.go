//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestBenchMetricsStagedBytes pins the staging probe on both places Nextflow
// writes a task's input links: inline in nxf_stage, and in .command.stage,
// which nxf_stage runs once the links reach the wrapper's stage-file
// threshold (NXF_WRAPPER_STAGE_FILE_THRESHOLD, 1 MB).
func TestBenchMetricsStagedBytes(t *testing.T) {
	requireTools(t, "python3")

	dir := t.TempDir()
	inline := writeBenchFile(t, filepath.Join(dir, "inline.txt"), "abc")
	spilled := writeBenchFile(t, filepath.Join(dir, "spilled.txt"), "abcde")

	writeBenchTask(t, dir, "aa/inline", "WF:ST (1)", "    rm -f inline.txt\n    ln -s "+inline+" inline.txt\n")

	spill := filepath.Join(dir, "work", "bb", "spilled")
	writeBenchTask(t, dir, "bb/spilled", "WF:ST (2)", "    bash "+filepath.Join(spill, ".command.stage")+"\n")
	writeBenchFile(t, filepath.Join(spill, ".command.stage"), "rm -f spilled.txt\nln -s "+spilled+" spilled.txt\n")

	trace := writeBenchFile(t, filepath.Join(dir, "trace.tsv"), "name\nWF:ST (1)\nWF:ST (2)\n")
	stages := writeBenchFile(t, filepath.Join(dir, "stages.json"), `["ST"]`)

	cmd := exec.Command("python3", filepath.Join(root, "test", "e2e", "bench_metrics.py"),
		"--name", "x", "--trace", trace, "--workdir", dir, "--probe", "PROBE",
		"--producers", "1", "--stages", stages, "--mrp-jobs", "2")

	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("bench_metrics: %v", err)
	}

	type staging struct {
		Staged    int            `json:"stage_staged_bytes"`
		StagedMax map[string]int `json:"stage_staged_bytes_max"`
	}

	var got staging
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("bench_metrics output %q: %v", out, err)
	}

	want := staging{Staged: len("abc") + len("abcde"), StagedMax: map[string]int{"ST": len("abcde")}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("staging probe (-want +got):\n%s", diff)
	}
}

// writeBenchTask writes a task dir under dir/work whose .command.run names
// the task and holds nxfStage as the body of nxf_stage.
func writeBenchTask(t *testing.T, dir, hash, name, nxfStage string) {
	t.Helper()

	run := strings.Join([]string{"#!/bin/bash", "### name: '" + name + "'", "nxf_stage() {", "    true", nxfStage + "}", ""}, "\n")
	writeBenchFile(t, filepath.Join(dir, "work", hash, ".command.run"), run)
}

// writeBenchFile writes body to path, creating its dir, and returns path.
func writeBenchFile(t *testing.T, path, body string) string {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	return path
}
