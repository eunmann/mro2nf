package shim

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/types"
)

// orphanStage is a py stage whose main starts a background child that inherits
// the stage's stdout/stderr and outlives it, recording the child's pid in
// bg.pid in its cwd (the files dir).
const orphanStage = `import subprocess

def main(args, outs):
    p = subprocess.Popen(["sleep", "60"])
    with open("bg.pid", "w") as f:
        f.write(str(p.pid))
`

// orphanExec is an exec stage script doing the same from sh; its args are
// phase, meta, files, journal.
const orphanExec = `sleep 60 & echo $! > "$3/bg.pid"; exit 0`

// TestPhaseReturnsWhileOrphanHoldsStdout runs a stage that exits while a
// background child it started still holds its stdout/stderr. mrp hands the
// job real files (jobmanager_local.go executeLocal), so its Wait returns when
// the stage exits; the phase must likewise return, not block until the
// orphan exits.
func TestPhaseReturnsWhileOrphanHoldsStdout(t *testing.T) {
	for _, lang := range []ir.Lang{ir.LangPy, ir.LangExec} {
		t.Run(string(lang), func(t *testing.T) {
			adapter := orphanAdapter(t, lang)
			work := t.TempDir()
			t.Cleanup(func() { killRecordedPid(t, filepath.Join(work, "main", "files", "bg.pid")) })

			done := make(chan error, 1)

			go func() {
				_, err := RunMain(context.Background(), work, adapter, json.RawMessage(`{}`), ChunkDef{},
					nil, types.NewTable(nil), Resources{Threads: 1, MemGB: 1}, Invocation{Stage: "ORPHAN"})
				done <- err
			}()

			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("RunMain: %v", err)
				}
			case <-time.After(20 * time.Second):
				t.Fatal("RunMain still blocked 20s after the stage exited: the orphan's stdout kept it waiting")
			}
		})
	}
}

// orphanAdapter returns the lang adapter for the orphan-leaving stage.
func orphanAdapter(t *testing.T, lang ir.Lang) Adapter {
	t.Helper()

	for _, tool := range []string{"python3", "sh", "sleep"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}

	if lang == ir.LangExec {
		return Adapter{Lang: ir.LangExec, Stagecode: "/bin/sh", SrcArgs: []string{"-c", orphanExec, "orphan"}}
	}

	code := filepath.Join(t.TempDir(), "orphan")
	if err := os.MkdirAll(code, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(code, "__init__.py"), []byte(orphanStage), 0o644); err != nil {
		t.Fatal(err)
	}

	shell, err := filepath.Abs("../../vendor-martian/python/martian_shell.py")
	if err != nil {
		t.Fatal(err)
	}

	return Adapter{Lang: ir.LangPy, Shell: shell, Stagecode: code}
}

// killRecordedPid kills the process whose pid the stage wrote to path, so the
// orphan does not outlive the test.
func killRecordedPid(t *testing.T, path string) {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Logf("no orphan pid recorded: %v", err)

		return
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Errorf("orphan pid %q: %v", data, err)

		return
	}

	_ = syscall.Kill(pid, syscall.SIGKILL)
}
