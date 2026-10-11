package shim

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/types"
)

// alarmStage raises two alarms, the second spanning two lines.
const alarmStage = `import martian

def main(args, outs):
    martian.alarm("low read count")
    martian.alarm("ambient RNA high\nin 3 of 8 lanes")
`

// TestPhaseForwardsAlarms runs stages that raise martian.alarm messages. mrp
// prints each completed stage's _alarm under "Alerts for <stage>:"
// (core/stage.go doComplete); the phase must surface every alarm line on the
// task's stderr rather than leave it in the scratch _alarm file.
func TestPhaseForwardsAlarms(t *testing.T) {
	for _, tc := range []struct {
		lang    ir.Lang
		adapter func(t *testing.T) Adapter
	}{
		{ir.LangPy, alarmPyAdapter},
		{ir.LangExec, alarmExecAdapter},
	} {
		t.Run(string(tc.lang), func(t *testing.T) {
			adapter := tc.adapter(t)

			var runErr error

			got := captureStderr(t, func() {
				_, runErr = RunMain(context.Background(), t.TempDir(), adapter, json.RawMessage(`{}`), ChunkDef{},
					nil, types.NewTable(nil), Resources{Threads: 1, MemGB: 1}, Invocation{Stage: "QC"})
			})
			if runErr != nil {
				t.Fatalf("RunMain: %v", runErr)
			}

			for _, want := range []string{
				"mre: ALARM: QC main: low read count\n",
				"mre: ALARM: QC main: ambient RNA high\n",
				"mre: ALARM: QC main: in 3 of 8 lanes\n",
			} {
				if !strings.Contains(got, want) {
					t.Errorf("stderr lacks %q:\n%s", want, got)
				}
			}
		})
	}
}

func alarmPyAdapter(t *testing.T) Adapter {
	t.Helper()

	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}

	code := filepath.Join(t.TempDir(), "qc")
	if err := os.MkdirAll(code, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(code, "__init__.py"), []byte(alarmStage), 0o644); err != nil {
		t.Fatal(err)
	}

	shell, err := filepath.Abs("../../vendor-martian/python/martian_shell.py")
	if err != nil {
		t.Fatal(err)
	}

	return Adapter{Lang: ir.LangPy, Shell: shell, Stagecode: code}
}

// alarmExecAdapter is an exec stage appending to _alarm itself, as mrjob and
// the compiled-stage adapters do; its args are phase, meta, files, journal.
func alarmExecAdapter(t *testing.T) Adapter {
	t.Helper()

	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	script := `printf 'low read count\nambient RNA high\nin 3 of 8 lanes\n' >> "$2/_alarm"`

	return Adapter{Lang: ir.LangExec, Stagecode: "/bin/sh", SrcArgs: []string{"-c", script, "qc"}}
}
