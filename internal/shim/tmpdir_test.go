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

// TestAdapterThreadEnvs pins #247: like mrp (core/jobmanager.go threadEnvs),
// the stage child runs with every jobmanagers/config.json thread_envs variable
// set to ceil(threads), overriding a value inherited from the environment.
func TestAdapterThreadEnvs(t *testing.T) {
	t.Setenv("OMP_NUM_THREADS", "96")

	work := t.TempDir()
	script := filepath.Join(work, "stage.sh")
	body := "#!/bin/sh\nprintf '{\"v\":\"%s %s %s %s %s %s %s %s %s\"}' " +
		"\"$GOMAXPROCS\" \"$MKL_NUM_THREADS\" \"$NUMEXPR_MAX_THREADS\" \"$NUMEXPR_NUM_THREADS\" " +
		"\"$OMP_NUM_THREADS\" \"$OMP_THREAD_LIMIT\" \"$POLARS_MAX_THREADS\" \"$ROOT_MAX_THREADS\" " +
		"\"$TF_NUM_THREADS\" > \"$2/_outs\"\n"

	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	a := Adapter{Lang: ir.LangExec, Stagecode: script}
	chunk := ChunkDef{Args: map[string]json.RawMessage{}}

	outs, err := RunMain(context.Background(), work, a, nil, chunk,
		nil, types.NewTable(nil), Resources{Threads: 1.5, MemGB: 1}, Invocation{Call: "T"})
	if err != nil {
		t.Fatalf("RunMain: %v", err)
	}

	var got struct {
		V string `json:"v"`
	}

	if err := json.Unmarshal(outs, &got); err != nil {
		t.Fatalf("parse outs %q: %v", outs, err)
	}

	if want := "2 2 2 2 2 2 2 2 2"; got.V != want {
		t.Errorf("thread envs = %q, want %q (ceil(1.5) for each, overriding the inherited 96)", got.V, want)
	}
}
