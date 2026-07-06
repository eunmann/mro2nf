package shim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/types"
)

// scalarOuts builds non-file (float) output params, whose _outs skeleton is all
// null — matching these tests' pre-bug behavior — paired with an empty table.
func scalarOuts(names ...string) []ir.Param {
	out := make([]ir.Param, len(names))
	for i, n := range names {
		out[i] = ir.Param{Name: n, BaseType: "float"}
	}

	return out
}

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

	defs, _, err := RunSplit(ctx, filepath.Join(work, "split"), adapter, stageArgs, res, inv)
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
			adapter, stageArgs, def, scalarOuts("sum", "square"), types.NewTable(nil), res, inv,
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
		defs, chunkOuts, scalarOuts("sum"), types.NewTable(nil), res, inv,
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

	defs, _, err := RunSplit(ctx, filepath.Join(work, "split"), adapter, stageArgs, res, inv)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if len(defs) != 1 {
		t.Fatalf("chunks = %d, want 1", len(defs))
	}

	mainDir := filepath.Join(work, "chnk")
	if _, err := RunMain(ctx, mainDir, adapter, stageArgs, defs[0], scalarOuts("sum", "square"), types.NewTable(nil), res, inv); err != nil {
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

// TestSplitJobInfoResolvesNegativeSentinel checks the split phase reports the
// magnitude of a negative adaptive sentinel in _jobinfo, symmetric with main and
// join — a split function's get_memory_allocation() must see |x|, not the raw
// negative.
func TestSplitJobInfoResolvesNegativeSentinel(t *testing.T) {
	adapter := sumSquaresAdapter(t)
	inv := Invocation{Call: "P", Args: json.RawMessage(`{"values":[2]}`), MROFile: "p.mro"}
	stageArgs := json.RawMessage(`{"values":[2]}`)
	work := t.TempDir()

	res := Resources{Threads: -1, MemGB: -8}
	if _, _, err := RunSplit(context.Background(), work, adapter, stageArgs, res, inv); err != nil {
		t.Fatalf("split: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(work, "split", "_jobinfo"))
	if err != nil {
		t.Fatalf("read _jobinfo: %v", err)
	}

	var info struct {
		MemGB   float64 `json:"memGB"`
		Threads float64 `json:"threads"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		t.Fatalf("parse _jobinfo: %v", err)
	}

	if info.MemGB != 8 || info.Threads != 1 {
		t.Errorf("split _jobinfo = {mem %v, threads %v}, want {8, 1} (sentinel magnitude)", info.MemGB, info.Threads)
	}
}

// TestReadStageDefsJoinOverride checks that a split's `{"join": {...}}` block is
// parsed into the join-phase resource override (Martian's mechanism for a split
// to request more resources for its gather), separate from the chunk defs.
func TestReadStageDefsJoinOverride(t *testing.T) {
	meta := t.TempDir()
	defsJSON := `{
		"chunks": [{"value": 1, "__threads": 1, "__mem_gb": 1}],
		"join": {"__threads": 2, "__mem_gb": 3, "__special": "highmem"}
	}`
	if err := writeRaw(filepath.Join(meta, "_stage_defs"), []byte(defsJSON)); err != nil {
		t.Fatalf("write _stage_defs: %v", err)
	}

	defs, join, err := readStageDefs(meta)
	if err != nil {
		t.Fatalf("readStageDefs: %v", err)
	}

	if len(defs) != 1 {
		t.Fatalf("chunks = %d, want 1", len(defs))
	}

	if join.Threads != 2 || join.MemGB != 3 || join.Special != "highmem" {
		t.Errorf("join override = %+v, want threads 2 / mem 3 / special highmem", join)
	}

	// A split with no join block yields a zero override (JOIN uses stage defaults).
	if err := writeRaw(filepath.Join(meta, "_stage_defs"), []byte(`{"chunks": []}`)); err != nil {
		t.Fatalf("write _stage_defs: %v", err)
	}

	if _, join, err = readStageDefs(meta); err != nil {
		t.Fatalf("readStageDefs: %v", err)
	}

	if (join != Resources{}) {
		t.Errorf("no-join override = %+v, want zero", join)
	}
}

// TestReadStageDefsJoinUnknownKey checks that a non-resource key in the join
// override is rejected, not silently dropped: mrp fails such a stage with
// "Invalid parameter in join definition", so honoring the JOIN with the intended
// resources depends on catching the malformed key here.
func TestReadStageDefsJoinUnknownKey(t *testing.T) {
	meta := t.TempDir()
	defsJSON := `{
		"chunks": [],
		"join": {"__mem_gb": 4, "value": 7, "extra": true}
	}`
	if err := writeRaw(filepath.Join(meta, "_stage_defs"), []byte(defsJSON)); err != nil {
		t.Fatalf("write _stage_defs: %v", err)
	}

	_, _, err := readStageDefs(meta)
	if !errors.Is(err, errJoinUnknownKey) {
		t.Fatalf("readStageDefs with unknown join keys: err = %v, want errJoinUnknownKey", err)
	}

	// The message must name the offending keys in sorted order for a usable error.
	if msg := err.Error(); !strings.Contains(msg, "extra") || !strings.Contains(msg, "value") {
		t.Errorf("error %q must name the unknown keys", msg)
	}
}

func TestMergeArgsNegativeResources(t *testing.T) {
	// A negative resource value is Martian's adaptive sentinel ("at least |x|").
	// It must override the phase default (not be discarded as "unset"), and it
	// resolves to its magnitude — mrp's cluster path negates it to positive before
	// the chunk runs, so the injected __keys carry |x|, not the raw sentinel.
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

	// 8/1 (the magnitude of the override), not 4 (the phase default it overrode).
	if string(got["__mem_gb"]) != "8" {
		t.Errorf("__mem_gb = %s, want 8 (|-8|, override resolved to magnitude)", got["__mem_gb"])
	}
	if string(got["__threads"]) != "1" {
		t.Errorf("__threads = %s, want 1 (|-1|)", got["__threads"])
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

// TestStageFailureClassification checks that an ASSERT-prefixed stage error is
// classified as non-retryable (ErrStageAssert) while any other failure is the
// ordinary retryable errStageFailed.
func TestStageFailureClassification(t *testing.T) {
	assert := stageFailure("main", "ASSERT: values must be non-empty")
	if !errors.Is(assert, ErrStageAssert) {
		t.Errorf("ASSERT error not classified as ErrStageAssert: %v", assert)
	}
	if errors.Is(assert, errStageFailed) {
		t.Error("ASSERT error should not also be errStageFailed")
	}

	boom := stageFailure("main", "boom: divide by zero")
	if !errors.Is(boom, errStageFailed) {
		t.Errorf("ordinary error not classified as errStageFailed: %v", boom)
	}
	if errors.Is(boom, ErrStageAssert) {
		t.Error("ordinary error should not be ErrStageAssert")
	}
}

// TestWrappedAdapterReadsAssert guards the comp/exec path's assert handling: a
// real mrjob writes a compiled-stage assertion to _assert (prefix stripped) and
// exits 0, so the wrapped path must read _assert — not only _errors + exit code —
// or the assertion is silently treated as success with stale outs.
func TestWrappedAdapterReadsAssert(t *testing.T) {
	if _, err := exec.LookPath("/bin/sh"); err != nil {
		t.Skip("/bin/sh not available")
	}

	meta := t.TempDir()
	files := t.TempDir()
	journal := filepath.Join(meta, "journal")

	// argv[1:] gets meta, files, journal appended, so inside sh: $1=meta. The
	// stand-in "mrjob" writes _assert and exits 0, exactly like the real one.
	argv := []string{"/bin/sh", "-c", `printf 'pipeline halted by assertion' > "$1/_assert"`, "mrjob"}

	err := runWrappedAdapter(context.Background(), meta, files, journal, Adapter{}, argv, "main", Resources{})
	if !errors.Is(err, ErrStageAssert) {
		t.Errorf("comp/exec assertion must surface as ErrStageAssert, got %v", err)
	}
}

// TestLimitedCommandMonitor checks that monitoring wraps the adapter in prlimit
// with the vmem ceiling, and is a no-op otherwise.
func TestLimitedCommandMonitor(t *testing.T) {
	ctx := context.Background()

	plain := limitedCommand(ctx, Adapter{Monitor: false}, 8, "python3", "stage.py")
	if filepath.Base(plain.Path) != "python3" {
		t.Errorf("unmonitored command = %q, want python3", plain.Path)
	}

	if _, err := exec.LookPath("prlimit"); err != nil {
		t.Skip("prlimit not available")
	}

	limited := limitedCommand(ctx, Adapter{Monitor: true}, 8, "python3", "stage.py")
	if filepath.Base(limited.Path) != "prlimit" {
		t.Fatalf("monitored command = %q, want prlimit wrapper", limited.Path)
	}

	wantAS := fmt.Sprintf("--as=%d", int64(8*bytesPerGB))
	if !slices.Contains(limited.Args, wantAS) {
		t.Errorf("monitored args %v missing %q", limited.Args, wantAS)
	}

	// Monitoring with no vmem ceiling does not wrap.
	if noVmem := limitedCommand(ctx, Adapter{Monitor: true}, 0, "python3"); filepath.Base(noVmem.Path) != "python3" {
		t.Errorf("monitor with vmem=0 wrapped: %q", noVmem.Path)
	}
}
