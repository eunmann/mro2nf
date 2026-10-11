package shim

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/types"
)

// TestPhaseFailureNamesStageAndPath fails a stage's main phase both ways an
// adapter can, and requires the error to name the stage, the phase and the
// metadata dir holding the phase's _errors/_stdout/_stderr, keeping the
// failure's class for the exit code.
func TestPhaseFailureNamesStageAndPath(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	for _, tc := range []struct {
		name, script string
		wantIs       error
	}{
		{"stage error", `printf 'reads file is empty' > "$2/_errors"`, errStageFailed},
		{"non-zero exit", `echo 'segfault in aligner' >&2; exit 3`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			work := t.TempDir()
			adapter := Adapter{Lang: ir.LangExec, Stagecode: "/bin/sh", SrcArgs: []string{"-c", tc.script, "align"}}

			silenceStdio(t)

			_, err := RunMain(context.Background(), work, adapter, json.RawMessage(`{}`), ChunkDef{},
				nil, types.NewTable(nil), Resources{Threads: 1, MemGB: 1}, Invocation{Stage: "ALIGN"})
			if err == nil {
				t.Fatal("RunMain passed, want the stage's failure")
			}

			for _, want := range []string{"stage ALIGN", "main phase", filepath.Join(work, "main")} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q lacks %q", err, want)
				}
			}

			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Errorf("error %v is not %v", err, tc.wantIs)
			}
		})
	}
}
