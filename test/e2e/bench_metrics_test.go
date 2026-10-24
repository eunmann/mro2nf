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

// TestBenchMetricsRestagedBytes pins the restaged-bytes probe: a file a task
// wrote counts when it is a hard link to, or a byte copy of, a file it
// staged, and not when it links to one or only shares its size.
func TestBenchMetricsRestagedBytes(t *testing.T) {
	requireTools(t, "python3")

	dir := t.TempDir()
	payload := writeBenchFile(t, filepath.Join(dir, "up", "f", "payload.txt"), "payload!")
	src := filepath.Dir(filepath.Dir(payload))

	task := filepath.Join(dir, "work", "aa", "use")
	writeBenchTask(t, dir, "aa/use", "WF:ST (1)", "    rm -f in\n    ln -s "+src+" in\n")

	if err := os.Symlink(src, filepath.Join(task, "in")); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(task, "args"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.Link(payload, filepath.Join(task, "args", "payload.txt")); err != nil {
		t.Fatal(err)
	}

	writeBenchFile(t, filepath.Join(task, "copy.txt"), "payload!")
	writeBenchFile(t, filepath.Join(task, "other.txt"), "another!")

	if err := os.Symlink(payload, filepath.Join(task, "link.txt")); err != nil {
		t.Fatal(err)
	}

	trace := writeBenchFile(t, filepath.Join(dir, "trace.tsv"), "name\nWF:ST (1)\n")
	stages := writeBenchFile(t, filepath.Join(dir, "stages.json"), `["ST"]`)

	out, err := exec.Command("python3", filepath.Join(root, "test", "e2e", "bench_metrics.py"),
		"--name", "x", "--trace", trace, "--workdir", dir, "--probe", "PROBE",
		"--producers", "1", "--stages", stages, "--mrp-jobs", "1").Output()
	if err != nil {
		t.Fatalf("bench_metrics: %v", err)
	}

	var got struct {
		Restaged int `json:"restaged_bytes"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("bench_metrics output %q: %v", out, err)
	}

	if want := 2 * len("payload!"); got.Restaged != want {
		t.Errorf("restaged_bytes = %d, want %d: the hard link and the byte copy", got.Restaged, want)
	}
}

// TestBenchMetricsReadsEveryTaskNameHeader pins the task name bench_metrics.py
// reads from each .command.run header Nextflow has written: a comment line,
// then a YAML header whose name is single-quoted up to 26.10.2 and a YAML
// double-quoted string, escapes and all, from 26.10.3.
func TestBenchMetricsReadsEveryTaskNameHeader(t *testing.T) {
	requireTools(t, "python3")

	cases := []struct {
		name   string
		header string
	}{
		{"comment", "# NEXTFLOW TASK: WF:ST (1)"},
		{"yaml single-quoted", "### name: 'WF:ST (1)'"},
		{"yaml double-quoted", `### name: "WF:ST (1)"`},
		{"yaml double-quoted with escapes", `### name: "WF:ST (a\"b\\c\t\u0001)"`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			in := writeBenchFile(t, filepath.Join(dir, "in.txt"), "abc")

			writeBenchTaskHeader(t, dir, "aa/task", tc.header, "    rm -f in.txt\n    ln -s "+in+" in.txt\n")

			got := benchStagedMax(t, dir, "WF:ST (1)")
			if diff := cmp.Diff(map[string]int{"ST": len("abc")}, got); diff != "" {
				t.Errorf("stage_staged_bytes_max (-want +got):\n%s", diff)
			}
		})
	}
}

// benchStagedMax runs bench_metrics.py over dir with stage process ST, whose
// trace holds the one task traced, and returns its stage_staged_bytes_max.
func benchStagedMax(t *testing.T, dir, traced string) map[string]int {
	t.Helper()

	trace := writeBenchFile(t, filepath.Join(dir, "trace.tsv"), "name\n"+traced+"\n")
	stages := writeBenchFile(t, filepath.Join(dir, "stages.json"), `["ST"]`)

	out, err := exec.Command("python3", filepath.Join(root, "test", "e2e", "bench_metrics.py"),
		"--name", "x", "--trace", trace, "--workdir", dir, "--probe", "PROBE",
		"--producers", "1", "--stages", stages, "--mrp-jobs", "1").Output()
	if err != nil {
		t.Fatalf("bench_metrics: %v", err)
	}

	var got struct {
		StagedMax map[string]int `json:"stage_staged_bytes_max"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("bench_metrics output %q: %v", out, err)
	}

	return got.StagedMax
}

// writeBenchTask writes a task dir under dir/work whose .command.run names
// the task and holds nxfStage as the body of nxf_stage.
func writeBenchTask(t *testing.T, dir, hash, name, nxfStage string) {
	t.Helper()

	writeBenchTaskHeader(t, dir, hash, "### name: '"+name+"'", nxfStage)
}

// writeBenchTaskHeader is writeBenchTask with the .command.run header line
// that names the task given whole.
func writeBenchTaskHeader(t *testing.T, dir, hash, header, nxfStage string) {
	t.Helper()

	run := strings.Join([]string{"#!/bin/bash", header, "nxf_stage() {", "    true", nxfStage + "}", ""}, "\n")
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
