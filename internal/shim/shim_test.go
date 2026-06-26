package shim

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/eunmann/martian-nextflow/internal/ir"
)

func sumSquaresAdapter(t *testing.T) Adapter {
	t.Helper()

	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}

	shell, err := filepath.Abs("../../vendor-martian/python/martian_shell.py")
	if err != nil {
		t.Fatalf("resolve shell: %v", err)
	}

	code, err := filepath.Abs("../../testdata/split_test/stages/sum_squares")
	if err != nil {
		t.Fatalf("resolve stagecode: %v", err)
	}

	return Adapter{Lang: ir.LangPy, Shell: shell, Stagecode: code}
}

// TestRunSumSquares drives the real Python adapter through split -> main x3 ->
// join and verifies the computed result, proving the shim speaks the Martian
// ABI correctly end-to-end.
func TestRunSumSquares(t *testing.T) {
	adapter := sumSquaresAdapter(t)
	res := Resources{Threads: 1, MemGB: 1, VMemGB: 4}
	inv := Invocation{
		Call:    "SUM_SQUARE_PIPELINE",
		Args:    json.RawMessage(`{"values":[1,2,3]}`),
		MROFile: "pipeline.mro",
	}
	stageArgs := json.RawMessage(`{"values":[1,2,3]}`)
	work := t.TempDir()
	ctx := context.Background()

	defs, err := RunSplit(ctx, filepath.Join(work, "split"), adapter, stageArgs, res, inv)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if len(defs) != 3 {
		t.Fatalf("chunks = %d, want 3", len(defs))
	}
	if defs[0].Resources.MemGB != 1 || defs[0].Resources.Threads != 1 {
		t.Errorf("chunk0 resources = %+v, want mem 1 / threads 1", defs[0].Resources)
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

		var got struct {
			Square float64 `json:"square"`
		}
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("parse chunk %d outs: %v", i, err)
		}

		want := float64((i + 1) * (i + 1))
		if got.Square != want {
			t.Errorf("chunk %d square = %v, want %v", i, got.Square, want)
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
		t.Errorf("sum = %v, want 14 (1+4+9)", final.Sum)
	}
}

// TestJobInfoResolvedResources checks that _jobinfo reports the per-chunk
// resolved allocation (what the stage actually got) rather than the raw phase
// request, matching mrp's golden _jobinfo.
func TestJobInfoResolvedResources(t *testing.T) {
	adapter := sumSquaresAdapter(t)
	inv := Invocation{Call: "P", Args: json.RawMessage(`{"values":[2]}`), MROFile: "p.mro"}
	stageArgs := json.RawMessage(`{"values":[2]}`)
	work := t.TempDir()
	ctx := context.Background()

	// The phase request is 2 GB, but the split assigns __mem_gb=1 per chunk.
	res := Resources{Threads: 1, MemGB: 2}

	defs, err := RunSplit(ctx, filepath.Join(work, "split"), adapter, stageArgs, res, inv)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if len(defs) != 1 {
		t.Fatalf("chunks = %d, want 1", len(defs))
	}

	mainDir := filepath.Join(work, "chnk")
	if _, err := RunMain(ctx, mainDir, adapter, stageArgs, defs[0], []string{"sum", "square"}, res, inv); err != nil {
		t.Fatalf("main: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(mainDir, "main", "_jobinfo"))
	if err != nil {
		t.Fatalf("read _jobinfo: %v", err)
	}

	var info struct {
		MemGB float64 `json:"memGB"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		t.Fatalf("parse _jobinfo: %v", err)
	}

	if info.MemGB != 1 {
		t.Errorf("_jobinfo memGB = %v, want 1 (chunk override, not phase request 2)", info.MemGB)
	}
}

func TestMergeArgsNegativeResources(t *testing.T) {
	// Martian uses negative resource values as adaptive sentinels; they must
	// override the phase default, not be discarded as "unset".
	chunk := ChunkDef{
		Args:      map[string]json.RawMessage{},
		Resources: Resources{MemGB: -8, Threads: -1},
	}

	merged, err := mergeArgs(json.RawMessage(`{}`), chunk, Resources{MemGB: 4, Threads: 4})
	if err != nil {
		t.Fatalf("mergeArgs: %v", err)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(merged, &got); err != nil {
		t.Fatalf("parse merged: %v", err)
	}

	if string(got["__mem_gb"]) != "-8" {
		t.Errorf("__mem_gb = %s, want -8 (negative sentinel preserved)", got["__mem_gb"])
	}
	if string(got["__threads"]) != "-1" {
		t.Errorf("__threads = %s, want -1", got["__threads"])
	}
}

func TestSpecialResourcePreserved(t *testing.T) {
	chunk := splitChunk(map[string]json.RawMessage{
		"value":     json.RawMessage("1"),
		"__special": json.RawMessage(`"highmem"`),
		"__mem_gb":  json.RawMessage("2"),
	})
	if chunk.Resources.Special != "highmem" {
		t.Fatalf("special not parsed from chunk def: %q", chunk.Resources.Special)
	}

	merged, err := mergeArgs(json.RawMessage(`{}`), chunk, Resources{})
	if err != nil {
		t.Fatalf("mergeArgs: %v", err)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(merged, &got); err != nil {
		t.Fatalf("parse merged: %v", err)
	}

	if string(got["__special"]) != `"highmem"` {
		t.Errorf("__special = %s, want \"highmem\"", got["__special"])
	}
}

func TestMergeArgsResources(t *testing.T) {
	chunk := ChunkDef{
		Args:      map[string]json.RawMessage{"value": json.RawMessage("2")},
		Resources: Resources{MemGB: 1, Threads: 1},
	}

	merged, err := mergeArgs(json.RawMessage(`{"values":[1,2,3]}`), chunk, Resources{MemGB: 1, Threads: 1})
	if err != nil {
		t.Fatalf("mergeArgs: %v", err)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(merged, &got); err != nil {
		t.Fatalf("parse merged: %v", err)
	}

	for key, want := range map[string]string{
		"value":     "2",
		"__mem_gb":  "1",
		"__threads": "1",
		"__vmem_gb": "4",
	} {
		if string(got[key]) != want {
			t.Errorf("merged[%q] = %s, want %s", key, got[key], want)
		}
	}
	if _, ok := got["values"]; !ok {
		t.Error("merged args should retain stage-level values")
	}
}
