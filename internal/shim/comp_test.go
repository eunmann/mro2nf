package shim

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/eunmann/martian-nextflow/internal/ir"
)

// TestRunSumSquaresComp exercises the comp adapter path (mrjob-wrapped binary
// that speaks the metadata protocol directly) through split -> main -> join.
func TestRunSumSquaresComp(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}

	mrjob, err := filepath.Abs("../../testdata/comp_fake/mrjob.sh")
	if err != nil {
		t.Fatalf("resolve mrjob: %v", err)
	}

	code, err := filepath.Abs("../../testdata/comp_fake/comp_sum.py")
	if err != nil {
		t.Fatalf("resolve stagecode: %v", err)
	}

	adapter := Adapter{Lang: ir.LangComp, Stagecode: code, Mrjob: mrjob}
	res := Resources{Threads: 1, MemGB: 1, VMemGB: 4}
	inv := Invocation{Call: "P", Args: json.RawMessage(`{"values":[1,2,3]}`), MROFile: "pipeline.mro"}
	stageArgs := json.RawMessage(`{"values":[1,2,3]}`)
	work := t.TempDir()
	ctx := context.Background()

	defs, _, err := RunSplit(ctx, filepath.Join(work, "split"), adapter, stageArgs, res, inv)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if len(defs) != 3 {
		t.Fatalf("chunks = %d, want 3", len(defs))
	}

	chunkOuts := make([]json.RawMessage, 0, len(defs))
	for i, def := range defs {
		out, err := RunMain(
			ctx, filepath.Join(work, fmt.Sprintf("chnk%d", i)),
			adapter, stageArgs, def, []string{"sum", "square"}, res, inv,
		)
		if err != nil {
			t.Fatalf("main chunk %d: %v", i, err)
		}
		chunkOuts = append(chunkOuts, out)
	}

	finalRaw, err := RunJoin(
		ctx, filepath.Join(work, "join"), adapter, stageArgs,
		defs, chunkOuts, []string{"sum"}, res, inv,
	)
	if err != nil {
		t.Fatalf("join: %v", err)
	}

	var final struct {
		Sum float64 `json:"sum"`
	}
	if err := json.Unmarshal(finalRaw, &final); err != nil {
		t.Fatalf("parse join outs: %v", err)
	}
	if final.Sum != 14 {
		t.Errorf("sum = %v, want 14", final.Sum)
	}
}
