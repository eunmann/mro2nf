package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWarnOutputScratchRefs checks the producer-side guard emits a loud stderr
// warning (forwarded to the task log) when a stage output bakes this task's
// absolute work-dir path into file content, and stays silent for a clean output.
func TestWarnOutputScratchRefs(t *testing.T) {
	work := t.TempDir()
	abs, err := filepath.Abs(work)
	if err != nil {
		t.Fatalf("abs: %v", err)
	}

	fdir := filepath.Join(work, "outs", "f")
	if err := os.MkdirAll(fdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	baked := abs + "/main/files/report.txt"
	if err := os.WriteFile(filepath.Join(fdir, "L0000.json"),
		[]byte(`{"report": "`+baked+`"}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	out := captureStderr(t, func() {
		warnOutputScratchRefs(filepath.Join(work, "outs"), work)
	})

	if !strings.Contains(out, "WARNING") || !strings.Contains(out, baked) {
		t.Errorf("expected a loud warning naming %q, got:\n%s", baked, out)
	}

	if !strings.Contains(out, filepath.Join("f", "L0000.json")) {
		t.Errorf("warning should name the offending output file, got:\n%s", out)
	}

	// A clean output produces no warning.
	if err := os.WriteFile(filepath.Join(fdir, "L0000.json"),
		[]byte(`{"report": "report.txt", "n": 5}`), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	if clean := captureStderr(t, func() {
		warnOutputScratchRefs(filepath.Join(work, "outs"), work)
	}); clean != "" {
		t.Errorf("clean output must not warn, got:\n%s", clean)
	}
}

// captureStderr redirects os.Stderr around fn and returns what it wrote.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}

	orig := os.Stderr
	os.Stderr = w

	fn()

	_ = w.Close()
	os.Stderr = orig

	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	return string(data)
}
