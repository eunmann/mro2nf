//go:build e2e

package e2e

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestSaveRunLogsKeepsFailedTasks builds a project after a failed run and
// requires saveRunLogs to keep the run's logs and the logs of the tasks that
// failed or never finished, not of the tasks that passed, each capped.
func TestSaveRunLogsKeepsFailedTasks(t *testing.T) {
	proj, dst := t.TempDir(), t.TempDir()

	big := strings.Repeat("x", keptLogMaxBytes) + "Command error: boom\n"

	files := map[string]string{
		"run.log":                        "ERROR ~ Error executing process > 'STAGE (1)'\n",
		".nextflow.log":                  "driver log\n",
		"trace-20261011-1.txt":           "task_id\thash\n",
		"main.nf":                        "workflow {}\n",
		"work/ab/failed/.command.sh":     "mre main\n",
		"work/ab/failed/.command.err":    big,
		"work/ab/failed/.exitcode":       "1\n",
		"work/cd/aborted/.command.sh":    "mre main\n",
		"work/cd/aborted/.command.log":   "killed\n",
		"work/ef/passed/.command.sh":     "mre main\n",
		"work/ef/passed/.command.err":    "fine\n",
		"work/ef/passed/.exitcode":       "0",
		"work/ef/passed/main/_outs":      "{}",
		"work/ab/failed/main/files/data": "payload",
	}

	for rel, body := range files {
		writeTestFile(t, filepath.Join(proj, rel), body)
	}

	if err := saveRunLogs(proj, dst); err != nil {
		t.Fatalf("saveRunLogs: %v", err)
	}

	want := []string{
		".nextflow.log", "run.log", "trace-20261011-1.txt",
		"work/ab/failed/.command.err", "work/ab/failed/.command.sh", "work/ab/failed/.exitcode",
		"work/cd/aborted/.command.log", "work/cd/aborted/.command.sh",
	}

	if diff := cmp.Diff(want, listFiles(t, dst)); diff != "" {
		t.Errorf("kept files (-want +got):\n%s", diff)
	}

	kept, err := os.ReadFile(filepath.Join(dst, "work/ab/failed/.command.err"))
	if err != nil {
		t.Fatal(err)
	}

	if len(kept) != keptLogMaxBytes || !strings.HasSuffix(string(kept), "Command error: boom\n") {
		t.Errorf("kept .command.err is %d bytes ending %q, want its last %d bytes", len(kept), kept[max(len(kept)-30, 0):], keptLogMaxBytes)
	}
}

// errRunFailed stands in for a failed `nextflow run`'s exit.
var errRunFailed = errors.New("exit status 1")

// TestNextflowRunErrorCarriesErrorReport requires a failed run's error to
// carry the last runLogTailLines of output, which hold Nextflow's whole
// error report rather than its last few lines.
func TestNextflowRunErrorCarriesErrorReport(t *testing.T) {
	lines := make([]string, 0, 60)
	for i := range 60 {
		lines = append(lines, fmt.Sprintf("line %02d", i))
	}

	err := nextflowRunError(errRunFailed, []byte(strings.Join(lines, "\n")+"\n"))
	if err == nil {
		t.Fatal("no error for a failed run")
	}

	for i, l := range lines {
		if got, want := strings.Contains(err.Error(), l), i >= len(lines)-runLogTailLines; got != want {
			t.Errorf("error carries %q: %v, want %v", l, got, want)
		}
	}

	if err := nextflowRunError(nil, []byte("ok\n")); err != nil {
		t.Errorf("a passing run's error = %v, want nil", err)
	}
}

func writeTestFile(t *testing.T, path, body string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// listFiles is every regular file under dir, relative and sorted.
func listFiles(t *testing.T, dir string) []string {
	t.Helper()

	var got []string

	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}

		rel, err := filepath.Rel(dir, path)
		got = append(got, rel)

		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	slices.Sort(got)

	return got
}
