package shim

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// bigStderr is larger than any tail a failure message may carry.
const bigStderr = 16 << 20

// TestFailureCarriesBoundedStderrTail fails a phase whose _stderr is 16 MiB:
// the error must carry only a bounded tail ending in the stage's last line,
// and building it must not read the whole file into memory (#336).
func TestFailureCarriesBoundedStderrTail(t *testing.T) {
	t.Run("py", func(t *testing.T) {
		meta := t.TempDir()
		body := append(bytes.Repeat([]byte("noise line\n"), bigStderr/11), "the real failure\n"...)

		if err := os.WriteFile(filepath.Join(meta, "_stderr"), body, filePerm); err != nil {
			t.Fatal(err)
		}

		checkBoundedFailure(t, func() error {
			return classifyAdapterResult("main", meta, nil, nil, errWaitFail, nil)
		})
	})

	t.Run("exec", func(t *testing.T) {
		if _, err := exec.LookPath("sh"); err != nil {
			t.Skip("sh not available")
		}

		meta, files := t.TempDir(), t.TempDir()
		script := `head -c 16777216 /dev/zero | tr '\0' 'x' >&2; printf '\nthe real failure\n' >&2; exit 3`
		argv := []string{"/bin/sh", "-c", script, "stage"}

		silenceStdio(t)
		checkBoundedFailure(t, func() error {
			return runWrappedAdapter(context.Background(), meta, files, filepath.Join(meta, "journal"), Adapter{}, argv, "main", Resources{})
		})
	})
}

// checkBoundedFailure runs fail and checks its error ends in the stage's last
// stderr line, is bounded, and cost no allocation near the file's size.
func checkBoundedFailure(t *testing.T, fail func() error) {
	t.Helper()

	var before, after runtime.MemStats

	runtime.GC()
	runtime.ReadMemStats(&before)

	err := fail()

	runtime.ReadMemStats(&after)

	if err == nil {
		t.Fatal("phase passed, want the stage's non-zero exit")
	}

	msg := err.Error()
	if !strings.HasSuffix(msg, "the real failure") {
		t.Errorf("error does not end in the stage's last stderr line: ...%q", msg[max(len(msg)-80, 0):])
	}

	if len(msg) > stderrTailBytes+256 {
		t.Errorf("error is %d bytes, want at most the %d-byte tail plus context", len(msg), stderrTailBytes)
	}

	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > bigStderr/4 {
		t.Errorf("failing the phase allocated %d bytes for a %d-byte _stderr", alloc, bigStderr)
	}
}

// silenceStdio points the shim's stdout/stderr, which the stage's output is
// teed to, at /dev/null for the test.
func silenceStdio(t *testing.T) {
	t.Helper()

	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}

	stdout, stderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = null, null

	t.Cleanup(func() {
		os.Stdout, os.Stderr = stdout, stderr
		_ = null.Close()
	})
}
