//go:build e2e

package e2e

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// e2eArtifactsDefault is where a failed test keeps its run logs unless
// E2E_ARTIFACTS names another dir; PR Validation uploads it on failure.
const e2eArtifactsDefault = "artifacts/e2e"

// keptLogMaxBytes caps each kept log at its last bytes.
const keptLogMaxBytes = 1 << 20

// keptProjects holds the test+project pairs keepRunLogsOnFailure registered.
var keptProjects sync.Map

// keepRunLogsOnFailure arranges for proj's run logs to be copied under the
// artifacts dir if t fails, before t.TempDir deletes them: cleanups run last
// registered first, and proj was created before the run.
func keepRunLogsOnFailure(t *testing.T, proj string) {
	t.Helper()

	if _, seen := keptProjects.LoadOrStore(t.Name()+"\x00"+proj, true); seen {
		return
	}

	t.Cleanup(func() {
		if !t.Failed() {
			return
		}

		dst := filepath.Join(rootPath(envOr("E2E_ARTIFACTS", e2eArtifactsDefault)), artifactPath(t.Name()), filepath.Base(proj))
		if err := saveRunLogs(proj, dst); err != nil {
			t.Logf("keep run logs of %s: %v", proj, err)

			return
		}

		t.Logf("run logs kept in %s", dst)
	})
}

var unsafeArtifactChar = regexp.MustCompile(`[^A-Za-z0-9._/-]`)

// artifactPath is a test name as a relative path upload-artifact accepts:
// subtests nest as dirs and every other unusual character becomes _.
func artifactPath(name string) string {
	return unsafeArtifactChar.ReplaceAllString(strings.ReplaceAll(name, "..", "_"), "_")
}

// saveRunLogs copies proj's run.log, .nextflow.log and trace files, and the
// logs of every task that did not exit 0, into dst.
func saveRunLogs(proj, dst string) error {
	var names []string

	for _, pattern := range []string{"run.log", ".nextflow.log", "trace*.txt"} {
		m, err := filepath.Glob(filepath.Join(proj, pattern))
		if err != nil {
			return fmt.Errorf("glob %s: %w", pattern, err)
		}

		names = append(names, m...)
	}

	tasks, err := failedTaskDirs(filepath.Join(proj, "work"))
	if err != nil {
		return err
	}

	for _, dir := range tasks {
		for _, f := range []string{".command.sh", ".command.out", ".command.err", ".command.log", ".exitcode"} {
			names = append(names, filepath.Join(dir, f))
		}
	}

	for _, src := range names {
		rel, err := filepath.Rel(proj, src)
		if err != nil {
			return fmt.Errorf("relate %s: %w", src, err)
		}

		if err := copyTail(src, filepath.Join(dst, rel)); err != nil {
			return err
		}
	}

	return nil
}

// failedTaskDirs lists the task dirs under work (work/xx/hash) whose
// .exitcode is missing or not 0: the tasks that failed or never finished.
func failedTaskDirs(work string) ([]string, error) {
	dirs, err := filepath.Glob(filepath.Join(work, "*", "*"))
	if err != nil {
		return nil, fmt.Errorf("glob task dirs: %w", err)
	}

	var failed []string

	for _, dir := range dirs {
		if !fileExists(filepath.Join(dir, ".command.sh")) {
			continue
		}

		code, err := os.ReadFile(filepath.Join(dir, ".exitcode"))
		if err != nil || strings.TrimSpace(string(code)) != "0" {
			failed = append(failed, dir)
		}
	}

	return failed, nil
}

// copyTail copies at most the last keptLogMaxBytes of src to dst; a src that
// does not exist is skipped.
func copyTail(src, dst string) error {
	in, err := os.Open(src)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()

	info, err := in.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", src, err)
	}

	if _, err := in.Seek(max(info.Size()-keptLogMaxBytes, 0), io.SeekStart); err != nil {
		return fmt.Errorf("seek %s: %w", src, err)
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(dst), err)
	}

	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}

	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()

		return fmt.Errorf("copy %s: %w", src, err)
	}

	if err := out.Close(); err != nil {
		return fmt.Errorf("close %s: %w", dst, err)
	}

	return nil
}
