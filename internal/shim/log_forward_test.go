package shim

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureStderr swaps os.Stderr for a pipe, runs fn, and returns what fn wrote
// to stderr. forwardStageLog writes there directly (the shim has no injectable
// writer), so the test captures the real fd.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	orig := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = orig }()

	fn()

	_ = w.Close()

	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}

	return string(out)
}

// TestForwardStageLog checks that a stage's _log is teed to the shim's stderr,
// so it survives in the task's captured logs on an object-store backend that
// does not retain the per-task scratch directory.
func TestForwardStageLog(t *testing.T) {
	meta := t.TempDir()
	if err := os.WriteFile(filepath.Join(meta, "_log"), []byte("hello from stage\nsecond line\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := captureStderr(t, func() { forwardStageLog(meta) })

	for _, want := range []string{"martian stage log", "hello from stage", "second line"} {
		if !strings.Contains(got, want) {
			t.Errorf("forwarded stderr = %q, want it to contain %q", got, want)
		}
	}
}

// TestForwardStageLogSilent checks the best-effort cases: a missing log and an
// empty/whitespace-only log both produce no output (no spurious header).
func TestForwardStageLogSilent(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		got := captureStderr(t, func() { forwardStageLog(t.TempDir()) })
		if got != "" {
			t.Errorf("missing _log forwarded %q, want nothing", got)
		}
	})

	t.Run("empty", func(t *testing.T) {
		meta := t.TempDir()
		if err := os.WriteFile(filepath.Join(meta, "_log"), []byte("  \n\t\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		got := captureStderr(t, func() { forwardStageLog(meta) })
		if got != "" {
			t.Errorf("empty _log forwarded %q, want nothing", got)
		}
	})
}
