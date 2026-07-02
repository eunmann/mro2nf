package shim

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/types"
)

// TestAdapterTMPDIR checks the stage child runs with TMPDIR pointed at the
// phase's per-stage tmp dir (mrp's per-job TMPDIR contract), and that the dir
// exists before the stage starts.
func TestAdapterTMPDIR(t *testing.T) {
	work := t.TempDir()

	// An exec-adapter stand-in: argv is <stagecode> <phase> <meta> <files>
	// <journal>, so $2 is the metadata dir. It fails unless TMPDIR exists and
	// reports the value through _outs.
	script := filepath.Join(work, "stage.sh")
	body := "#!/bin/sh\n[ -d \"$TMPDIR\" ] || exit 3\nprintf '{\"t\":\"%s\"}' \"$TMPDIR\" > \"$2/_outs\"\n"

	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	a := Adapter{Lang: ir.LangExec, Stagecode: script}
	chunk := ChunkDef{Args: map[string]json.RawMessage{}}

	outs, err := RunMain(context.Background(), work, a, nil, chunk,
		nil, types.NewTable(nil), Resources{Threads: 1, MemGB: 1}, Invocation{Call: "T"})
	if err != nil {
		t.Fatalf("RunMain: %v", err)
	}

	var got struct {
		T string `json:"t"`
	}

	if err := json.Unmarshal(outs, &got); err != nil {
		t.Fatalf("parse outs %q: %v", outs, err)
	}

	meta, err := filepath.Abs(filepath.Join(work, "main"))
	if err != nil {
		t.Fatal(err)
	}

	if want := filepath.Join(meta, "tmp"); got.T != want {
		t.Errorf("TMPDIR = %q, want %q", got.T, want)
	}
}
