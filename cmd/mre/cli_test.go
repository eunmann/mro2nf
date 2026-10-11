package main

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/eunmann/mro2nf/internal/types"
	"github.com/google/go-cmp/cmp"
)

// writeTestBundle writes payload as a bundle directory with no file leaves.
func writeTestBundle(t *testing.T, dir string, payload map[string]any) {
	t.Helper()

	if err := shim.WriteBundle(dir, payload, nil, types.NewTable(nil), nil); err != nil {
		t.Fatalf("write bundle %s: %v", dir, err)
	}
}

// readTestBundle loads a bundle's payload into a generic map.
func readTestBundle(t *testing.T, dir string) map[string]any {
	t.Helper()

	raw, err := shim.ReadBundle(dir)
	if err != nil {
		t.Fatalf("read bundle %s: %v", dir, err)
	}

	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode bundle %s: %v", dir, err)
	}

	return out
}

// writeTestFile writes content to path and returns path.
func writeTestFile(t *testing.T, path, content string) string {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}

	return path
}

// readJSONFile decodes the JSON file at path into v.
func readJSONFile(t *testing.T, path string, v any) {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
}

// TestExitCode pins the exit-code mapping: an ASSERT-class stage failure (even
// wrapped) uses the distinct non-retryable code; anything else exits 1.
func TestExitCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "wrapped stage assert", err: fmt.Errorf("join: %w", shim.ErrStageAssert), want: shim.AssertExitCode},
		{name: "ordinary error", err: errUsage, want: 1},
		{name: "wrapped ordinary error", err: fmt.Errorf("read: %w", os.ErrNotExist), want: 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := exitCode(tc.err); got != tc.want {
				t.Errorf("exitCode(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

// TestCommonFlagsRequest covers the phase request flags: a static -special
// (#369) and an override that replaces the stage's and a chunk's memory
// (#248) but keeps the chunk's own __special.
func TestCommonFlagsRequest(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	cf := addCommon(fs)

	if err := fs.Parse([]string{"-memgb", "1.5", "-special", "gpu:2", "-overridememgb", "3"}); err != nil {
		t.Fatalf("parse flags: %v", err)
	}

	if diff := cmp.Diff(shim.Resources{Threads: 1, MemGB: 3, Special: "gpu:2"}, cf.resources()); diff != "" {
		t.Errorf("request mismatch (-want +got):\n%s", diff)
	}

	chunk := cf.overridden(shim.Resources{MemGB: 0.5, Special: "chunk"})
	if diff := cmp.Diff(shim.Resources{MemGB: 3, Special: "chunk"}, chunk); diff != "" {
		t.Errorf("overridden chunk mismatch (-want +got):\n%s", diff)
	}
}

// TestJoinRequestOverride pins the join half of #248 to mrp's getJobReqs:
// under {"join.mem_gb": 4} a join whose split returned __mem_gb 0.5 gets 4,
// and without an override it keeps the split's 0.5.
func TestJoinRequestOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "joinres.json")
	if err := os.WriteFile(path, []byte(`{"threads":2,"mem_gb":0.5}`), 0o644); err != nil {
		t.Fatalf("write joinres: %v", err)
	}

	tests := []struct {
		name string
		argv []string
		want shim.Resources
	}{
		{"override", []string{"-overridememgb", "4"}, shim.Resources{Threads: 2, MemGB: 4}},
		{"no override", nil, shim.Resources{Threads: 2, MemGB: 0.5}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			cf := addCommon(fs)

			if err := fs.Parse(tt.argv); err != nil {
				t.Fatalf("parse flags: %v", err)
			}

			got, err := cf.joinRequest(path)
			if err != nil {
				t.Fatalf("joinRequest: %v", err)
			}

			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("join request mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestCommonFlagsSrcArgs covers the -srcarg plumbing (#113): each repeated
// occurrence lands on Adapter.SrcArgs in order, and omitting the flag leaves
// SrcArgs empty.
func TestCommonFlagsSrcArgs(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	cf := addCommon(fs)

	argv := []string{"-stagecode", "/code/tool.py", "-lang", "exec", "-srcarg", "3", "-srcarg", "hello"}
	if err := fs.Parse(argv); err != nil {
		t.Fatalf("parse flags: %v", err)
	}

	want := shim.Adapter{Lang: ir.LangExec, Stagecode: "/code/tool.py", SrcArgs: []string{"3", "hello"}, Attempt: 1}
	if diff := cmp.Diff(want, cf.adapter()); diff != "" {
		t.Errorf("adapter mismatch (-want +got):\n%s", diff)
	}

	fs2 := flag.NewFlagSet("test", flag.ContinueOnError)
	cf2 := addCommon(fs2)

	if err := fs2.Parse(nil); err != nil {
		t.Fatalf("parse flags: %v", err)
	}

	if got := cf2.adapter().SrcArgs; len(got) != 0 {
		t.Errorf("SrcArgs without -srcarg = %v, want empty", got)
	}
}

// TestRunDispatch covers the top-level subcommand dispatch: no args is a usage
// error, an unknown phase wraps errUnknownPhase, and version succeeds.
func TestRunDispatch(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want error
	}{
		{name: "empty args", args: nil, want: errUsage},
		{name: "unknown phase", args: []string{"frobnicate"}, want: errUnknownPhase},
		{name: "version", args: []string{"version"}, want: nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := run(t.Context(), tc.args); !errors.Is(err, tc.want) {
				t.Errorf("run(%v) = %v, want %v", tc.args, err, tc.want)
			}
		})
	}
}

// TestBumpForkDim checks that every param gains exactly one outer fork dimension
// (MapDim for a map fork, ArrayDim for an array fork) and that the input slice
// is left unmutated.
func TestBumpForkDim(t *testing.T) {
	orig := []ir.Param{
		{Name: "a", BaseType: "txt", IsFile: true, ArrayDim: 1},
		{Name: "b", BaseType: "int", MapDim: 2},
	}

	tests := []struct {
		name    string
		mapFork bool
		want    []ir.Param
	}{
		{
			name: "array fork bumps ArrayDim",
			want: []ir.Param{
				{Name: "a", BaseType: "txt", IsFile: true, ArrayDim: 2},
				{Name: "b", BaseType: "int", ArrayDim: 1, MapDim: 2},
			},
		},
		{
			name:    "map fork bumps MapDim",
			mapFork: true,
			want: []ir.Param{
				{Name: "a", BaseType: "txt", IsFile: true, ArrayDim: 1, MapDim: 1},
				{Name: "b", BaseType: "int", MapDim: 3},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := []ir.Param{
				{Name: "a", BaseType: "txt", IsFile: true, ArrayDim: 1},
				{Name: "b", BaseType: "int", MapDim: 2},
			}

			got := bumpForkDim(in, tc.mapFork)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("bumpForkDim mismatch (-want +got):\n%s", diff)
			}

			if diff := cmp.Diff(orig, in); diff != "" {
				t.Errorf("input slice mutated (-want +got):\n%s", diff)
			}
		})
	}
}

// TestForkMetaRoundTrip round-trips the fork sidecars: forkmeta.json carries
// the fork count plus a JSON null keys field for an array fork (read back as
// nil keys) or the map fork's key array; forknames.json always lists the fork
// bundle dirs.
func TestForkMetaRoundTrip(t *testing.T) {
	names := []string{"fork_00000", "fork_00001"}

	tests := []struct {
		name     string
		keys     []string
		wantFile string
	}{
		{name: "array fork", keys: nil, wantFile: `{"count":2,"keys":null}`},
		{name: "map fork", keys: []string{"a", "b"}, wantFile: `{"count":2,"keys":["a","b"]}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := writeForkMeta(dir, names, forkMeta{Count: len(names), Keys: tc.keys}); err != nil {
				t.Fatalf("writeForkMeta: %v", err)
			}

			keysPath := filepath.Join(dir, "forkmeta.json")

			data, err := os.ReadFile(keysPath)
			if err != nil {
				t.Fatalf("read forkmeta.json: %v", err)
			}

			if string(data) != tc.wantFile {
				t.Errorf("forkmeta.json = %q, want %q", data, tc.wantFile)
			}

			got, err := readForkMeta(keysPath)
			if err != nil {
				t.Fatalf("readForkMeta: %v", err)
			}

			if diff := cmp.Diff(forkMeta{Count: len(names), Keys: tc.keys}, got); diff != "" {
				t.Errorf("fork meta round-trip mismatch (-want +got):\n%s", diff)
			}

			var gotNames []string

			readJSONFile(t, filepath.Join(dir, "forknames.json"), &gotNames)

			if diff := cmp.Diff(names, gotNames); diff != "" {
				t.Errorf("forknames.json mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestRawToMap covers the empty-payload tolerances (empty bytes, null, the empty
// string the adapter writes for a stage with no outputs), the not-an-object
// errors, and whole-number float fidelity via json.Number.
func TestRawToMap(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    map[string]any
		wantErr error
	}{
		{name: "empty object", raw: `{}`, want: map[string]any{}},
		{name: "padded null", raw: ` null `, want: map[string]any{}},
		{name: "empty json string", raw: `""`, want: map[string]any{}},
		{name: "empty bytes", raw: ``, want: map[string]any{}},
		{name: "array is not an object", raw: `[1]`, wantErr: errNotObject},
		{name: "number is not an object", raw: `5`, wantErr: errNotObject},
		{name: "whole-number float fidelity", raw: `{"x": 42.0}`, want: map[string]any{"x": json.Number("42.0")}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := rawToMap(json.RawMessage(tc.raw))
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("rawToMap(%q) error = %v, want %v", tc.raw, err, tc.wantErr)
				}

				return
			}

			if err != nil {
				t.Fatalf("rawToMap(%q): %v", tc.raw, err)
			}

			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("rawToMap(%q) mismatch (-want +got):\n%s", tc.raw, diff)
			}
		})
	}
}

// TestDecodeChunk checks that an empty raw payload (a non-split stage's single
// chunk) yields a non-nil empty Args map, and a real chunk payload decodes args
// plus resource overrides.
func TestDecodeChunk(t *testing.T) {
	empty, err := decodeChunk(nil)
	if err != nil {
		t.Fatalf("decodeChunk(nil): %v", err)
	}

	if empty.Args == nil || len(empty.Args) != 0 {
		t.Errorf("empty chunk Args = %v, want non-nil empty map", empty.Args)
	}

	got, err := decodeChunk(json.RawMessage(`{"args":{"n":1},"resources":{"threads":2}}`))
	if err != nil {
		t.Fatalf("decodeChunk: %v", err)
	}

	want := shim.ChunkDef{
		Args:      map[string]json.RawMessage{"n": json.RawMessage("1")},
		Resources: shim.Resources{Threads: 2},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("chunk mismatch (-want +got):\n%s", diff)
	}
}

// TestReadChunkDataResolvesFileLeaves is the #217 regression guard: the JOIN
// reads chunk DEFS from the staged per-chunk bundles, so a file-typed leaf the
// split phase created must come back rewritten to the locally-staged path (which
// exists), NOT the split worker's ephemeral scratch path. Before the fix the
// join read the unstaged chunks.json summary and got the split's absent path,
// so on an object-store backend the stage's join() open() failed. Local and
// docker-iso share a filesystem and cannot expose that, so this pins it in a
// unit test.
func TestReadChunkDataResolvesFileLeaves(t *testing.T) {
	tmp := t.TempDir()
	src := writeTestFile(t, filepath.Join(tmp, "ref_0.bin"), "10")

	// Stage the chunk def exactly as the split phase does (WriteChunkBundle →
	// MarkFiles copies the file leaf into the bundle and rewrites it to a marker).
	bundle := filepath.Join(tmp, "chunk_00000")
	def := shim.ChunkDef{Args: map[string]json.RawMessage{
		"chunk_id": json.RawMessage("0"),
		"ref":      json.RawMessage(`"` + src + `"`),
	}}
	tbl := types.NewTable(nil)
	if err := shim.WriteChunkBundle(bundle, def, []ir.Param{{Name: "ref", BaseType: "bin", IsFile: true}}, tbl, nil); err != nil {
		t.Fatalf("WriteChunkBundle: %v", err)
	}

	writeTestBundle(t, filepath.Join(tmp, "out_chunk_00000"), map[string]any{"part": 1})
	chunks := writeTestFile(t, filepath.Join(tmp, "chunks.json"), `[{"chunk_id":0}]`)

	defs, outs, err := readChunkData(chunks, tmp)
	if err != nil {
		t.Fatalf("readChunkData: %v", err)
	}

	if len(defs) != 1 || len(outs) != 1 {
		t.Fatalf("got %d defs / %d outs, want 1 / 1", len(defs), len(outs))
	}

	var got string
	if err := json.Unmarshal(defs[0].Args["ref"], &got); err != nil {
		t.Fatalf("decode ref leaf: %v", err)
	}

	if got == src {
		t.Fatalf("ref leaf was not restaged: still the split-scratch path %q (#217)", got)
	}

	if !strings.HasPrefix(got, bundle) {
		t.Errorf("ref leaf %q not resolved inside the staged bundle %q", got, bundle)
	}

	data, err := os.ReadFile(got)
	if err != nil {
		t.Fatalf("resolved ref leaf must exist and be readable on the join worker: %v", err)
	}

	if string(data) != "10" {
		t.Errorf("resolved ref content = %q, want %q", data, "10")
	}
}

// TestSplitComma checks trimming and empty-element dropping.
func TestSplitComma(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{name: "trims and drops empties", in: " a ,, b ,", want: []string{"a", "b"}},
		{name: "empty input", in: "", want: []string{}},
		{name: "single element", in: "x", want: []string{"x"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if diff := cmp.Diff(tc.want, splitComma(tc.in)); diff != "" {
				t.Errorf("splitComma(%q) mismatch (-want +got):\n%s", tc.in, diff)
			}
		})
	}
}

// TestReadInputs checks the id=bundleDir pair parsing: a pair without '=' is an
// errBadInput, and a good pair loads the referenced bundle's payload.
func TestReadInputs(t *testing.T) {
	if _, err := readInputs("nodelimiter"); !errors.Is(err, errBadInput) {
		t.Errorf("readInputs bad pair error = %v, want %v", err, errBadInput)
	}

	dir := filepath.Join(t.TempDir(), "outs")
	writeTestBundle(t, dir, map[string]any{"k": 9})

	got, err := readInputs("STAGE=" + dir)
	if err != nil {
		t.Fatalf("readInputs: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(got["STAGE"], &payload); err != nil {
		t.Fatalf("decode STAGE payload: %v", err)
	}

	if diff := cmp.Diff(map[string]any{"k": 9.0}, payload); diff != "" {
		t.Errorf("STAGE payload mismatch (-want +got):\n%s", diff)
	}
}

// TestProducerManifestNoTypes checks that a producer with no -types path loads
// an empty manifest (no file leaves to rewrite) instead of failing.
func TestProducerManifestNoTypes(t *testing.T) {
	prod := &producer{}

	man, err := prod.manifest()
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}

	if len(man.Structs) != 0 || len(man.Callables) != 0 {
		t.Errorf("empty producer manifest = %+v, want empty", man)
	}

	params, err := man.Params("ANY", types.RoleIn)
	if err != nil {
		t.Fatalf("Params on empty manifest: %v", err)
	}

	if params != nil {
		t.Errorf("empty manifest Params = %v, want nil", params)
	}
}

// TestProducerCoerceInputs checks the int-from-whole-float coercion of a bound
// int input against a manifest written to disk. A name only the split's chunk
// inputs declare keeps its float (#245), and a non-int value passes through.
func TestProducerCoerceInputs(t *testing.T) {
	man := types.Manifest{Callables: map[string]types.Callable{
		"S": {
			In:      []ir.Param{{Name: "x", Type: "int", BaseType: "int"}},
			ChunkIn: []ir.Param{{Name: "c", Type: "int", BaseType: "int"}},
		},
	}}
	path := filepath.Join(t.TempDir(), "types.json")
	if err := man.Write(path); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	prod := &producer{types: path, callable: "S"}

	raw, err := prod.coerceInputs(json.RawMessage(`{"x":5.0,"c":7.0,"other":1.5}`))
	if err != nil {
		t.Fatalf("coerceInputs: %v", err)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode coerced args: %v", err)
	}

	want := map[string]json.RawMessage{
		"x":     json.RawMessage("5"),
		"c":     json.RawMessage("7.0"),
		"other": json.RawMessage("1.5"),
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("coerced args mismatch (-want +got):\n%s", diff)
	}
}

// TestProducerValidateOuts checks a stage's final outputs fail the phase the way
// mrp fails the stage (#274): a struct output missing a declared key is an
// error naming it, a complete one passes, and a callable with no declared
// outputs (or no manifest) is not checked.
func TestProducerValidateOuts(t *testing.T) {
	man := types.Manifest{
		Structs: map[string]*ir.StructType{"S": {Name: "S", Fields: []ir.Param{
			{Name: "a", BaseType: "int"}, {Name: "b", BaseType: "int"},
		}}},
		Callables: map[string]types.Callable{
			"UP":   {Out: []ir.Param{{Name: "s", Type: "S", BaseType: "S"}}},
			"NONE": {},
		},
	}
	path := filepath.Join(t.TempDir(), "types.json")
	if err := man.Write(path); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	up := &producer{types: path, callable: "UP"}

	err := up.validateOuts(json.RawMessage(`{"s":{"a":1}}`))
	if !errors.Is(err, types.ErrInvalidOutputs) || !strings.Contains(err.Error(), "Expected S output value 's' missing key: b") {
		t.Errorf("validateOuts(missing key) = %v, want ErrInvalidOutputs naming key b", err)
	}

	if err := up.validateOuts(json.RawMessage(`{"s":{"a":1,"b":2}}`)); err != nil {
		t.Errorf("validateOuts(complete) = %v, want nil", err)
	}

	if err := (&producer{types: path, callable: "NONE"}).validateOuts(json.RawMessage(`""`)); err != nil {
		t.Errorf("validateOuts(no outputs) = %v, want nil", err)
	}

	if err := (&producer{callable: "UP"}).validateOuts(json.RawMessage(`{}`)); err != nil {
		t.Errorf("validateOuts(no manifest) = %v, want nil", err)
	}
}

// TestRunBindSmoke drives the bind subcommand end to end: a self ref resolves
// against the enclosing pipeline's args bundle into the output args bundle.
func TestRunBindSmoke(t *testing.T) {
	dir := t.TempDir()
	spec := writeTestFile(t, filepath.Join(dir, "spec.json"),
		`{"y":{"ref":{"kind":"self","id":"x","output":""}}}`)
	pipe := filepath.Join(dir, "pipeargs")
	writeTestBundle(t, pipe, map[string]any{"x": 7})
	out := filepath.Join(dir, "out")

	if err := run(t.Context(), []string{"bind", "-spec", spec, "-pipeargs", pipe, "-o", out}); err != nil {
		t.Fatalf("run bind: %v", err)
	}

	if diff := cmp.Diff(map[string]any{"y": 7.0}, readTestBundle(t, out)); diff != "" {
		t.Errorf("bound args mismatch (-want +got):\n%s", diff)
	}
}

// narrowManifest writes a manifest where DOWN takes `in S s` (struct S(a, b))
// and returns the -types flag value.
func narrowManifest(t *testing.T, dir string) string {
	t.Helper()

	man := types.Manifest{
		Structs: map[string]*ir.StructType{"S": {Name: "S", Fields: []ir.Param{
			{Name: "a", BaseType: "int"}, {Name: "b", BaseType: "int"},
		}}},
		Callables: map[string]types.Callable{"DOWN": {In: []ir.Param{{Name: "s", BaseType: "S"}}}},
	}

	path := filepath.Join(dir, "types.json")
	if err := man.Write(path); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	return path
}

// TestRunBindNarrowsStructs checks a struct bound to a narrower struct param is
// filtered to the declared fields in the args bundle, as mrp does (#239): a
// whole ref to UP {a,b,c} binds DOWN's `in S s` as {a,b}.
func TestRunBindNarrowsStructs(t *testing.T) {
	dir := t.TempDir()
	spec := writeTestFile(t, filepath.Join(dir, "spec.json"), `{"s":{"ref":{"kind":"call","id":"UP","output":""}}}`)
	up := filepath.Join(dir, "in_UP")
	writeTestBundle(t, up, map[string]any{"a": 1, "b": 2, "c": 3})
	out := filepath.Join(dir, "out")

	if err := run(t.Context(), []string{
		"bind", "-spec", spec, "-inputs", "UP=" + up, "-o", out,
		"-types", narrowManifest(t, dir), "-callable", "DOWN", "-role", types.RoleIn,
	}); err != nil {
		t.Fatalf("run bind: %v", err)
	}

	want := map[string]any{"s": map[string]any{"a": 1.0, "b": 2.0}}
	if diff := cmp.Diff(want, readTestBundle(t, out)); diff != "" {
		t.Errorf("bound args mismatch (-want +got):\n%s", diff)
	}
}

// TestRunForkBindNarrowsStructs checks every forkbind write path narrows a
// split struct element to the callee's declared struct (#239): the full-fork
// write, -index, and the pre-sliced -elementfile.
func TestRunForkBindNarrowsStructs(t *testing.T) {
	dir := t.TempDir()
	spec := writeTestFile(t, filepath.Join(dir, "spec.json"),
		`{"s":{"ref":{"kind":"self","id":"ws","output":""},"split":true}}`)
	pipe := filepath.Join(dir, "pipeargs")
	writeTestBundle(t, pipe, map[string]any{"ws": []any{map[string]any{"a": 1, "b": 2, "c": 3}}})
	typesFlag := []string{"-types", narrowManifest(t, dir), "-callable", "DOWN", "-role", types.RoleIn}
	base := []string{"forkbind", "-spec", spec, "-pipeargs", pipe}

	forks := filepath.Join(dir, "forks")
	byIndex := filepath.Join(dir, "by_index")
	byElem := filepath.Join(dir, "by_element")
	elem := writeTestFile(t, filepath.Join(dir, "e.json"), `{"a":1,"b":2,"c":3}`)

	for _, argv := range [][]string{
		{"-chunkdir", forks},
		{"-index", "0", "-o", byIndex},
		{"-elementfile", elem, "-o", byElem},
	} {
		if err := run(t.Context(), append(append(slices.Clone(base), argv...), typesFlag...)); err != nil {
			t.Fatalf("run forkbind %v: %v", argv, err)
		}
	}

	want := map[string]any{"s": map[string]any{"a": 1.0, "b": 2.0}}
	for _, got := range []string{filepath.Join(forks, "fork_00000"), byIndex, byElem} {
		if diff := cmp.Diff(want, readTestBundle(t, got)); diff != "" {
			t.Errorf("%s args mismatch (-want +got):\n%s", got, diff)
		}
	}
}

// TestRunForkBindArraySmoke drives forkbind in array mode over a literal split:
// one fork bundle per element, forknames.json listing them in order, and a
// forkmeta.json with the fork count and null keys.
func TestRunForkBindArraySmoke(t *testing.T) {
	dir := t.TempDir()
	spec := writeTestFile(t, filepath.Join(dir, "spec.json"), `{"n":{"literal":[1,2],"split":true}}`)
	forks := filepath.Join(dir, "forks")

	if err := run(t.Context(), []string{"forkbind", "-spec", spec, "-chunkdir", forks}); err != nil {
		t.Fatalf("run forkbind: %v", err)
	}

	for i, want := range []float64{1, 2} {
		got := readTestBundle(t, filepath.Join(forks, fmt.Sprintf("fork_%05d", i)))
		if diff := cmp.Diff(map[string]any{"n": want}, got); diff != "" {
			t.Errorf("fork %d args mismatch (-want +got):\n%s", i, diff)
		}
	}

	var names []string

	readJSONFile(t, filepath.Join(forks, "forknames.json"), &names)

	if diff := cmp.Diff([]string{"fork_00000", "fork_00001"}, names); diff != "" {
		t.Errorf("forknames.json mismatch (-want +got):\n%s", diff)
	}

	keysRaw, err := os.ReadFile(filepath.Join(forks, "forkmeta.json"))
	if err != nil {
		t.Fatalf("read forkmeta.json: %v", err)
	}

	if want := `{"count":2,"keys":null}`; string(keysRaw) != want {
		t.Errorf("forkmeta.json = %q, want %q", keysRaw, want)
	}
}

// TestRunForkBindIndex drives the native-scatter path (#76): -index writes only
// the one fork's args, identical to the corresponding full-fork write, and an
// out-of-range index is a loud error rather than a silent empty bundle.
func TestRunForkBindIndex(t *testing.T) {
	dir := t.TempDir()
	spec := writeTestFile(t, filepath.Join(dir, "spec.json"), `{"n":{"literal":[10,20,30],"split":true}}`)
	out := filepath.Join(dir, "args")

	if err := run(t.Context(), []string{"forkbind", "-spec", spec, "-index", "1", "-o", out}); err != nil {
		t.Fatalf("run forkbind -index: %v", err)
	}

	if diff := cmp.Diff(map[string]any{"n": 20.0}, readTestBundle(t, out)); diff != "" {
		t.Errorf("fork 1 args mismatch (-want +got):\n%s", diff)
	}

	// The native scatter relies on -index N being byte-identical to the default
	// path's fork_0000N: run the default, then diff fork_00001 against -o.
	forks := filepath.Join(dir, "forks")
	if err := run(t.Context(), []string{"forkbind", "-spec", spec, "-chunkdir", forks}); err != nil {
		t.Fatalf("run default forkbind: %v", err)
	}

	if diff := cmp.Diff(readTestBundle(t, filepath.Join(forks, "fork_00001")), readTestBundle(t, out)); diff != "" {
		t.Errorf("-index 1 not byte-identical to default fork_00001 (-default +index):\n%s", diff)
	}

	if err := run(t.Context(), []string{"forkbind", "-spec", spec, "-index", "5", "-o", out}); err == nil {
		t.Error("forkbind -index 5 over 3 forks must error, not write an empty bundle")
	}

	if err := run(t.Context(), []string{"forkbind", "-spec", spec, "-index", "0"}); err == nil {
		t.Error("forkbind -index without -o must error, not write to cwd")
	}
}

// TestRunForkBindElement drives the O(1) native-scatter path (#99): assembling
// a fork's args from a pre-sliced element must be byte-identical to the -index
// write of that fork (the driver hands the same value ResolveForks would slice),
// and a broadcast binding still resolves against pipeargs.
func TestRunForkBindElement(t *testing.T) {
	dir := t.TempDir()
	spec := writeTestFile(t, filepath.Join(dir, "spec.json"),
		`{"v":{"ref":{"kind":"self","id":"vals","output":""},"split":true},"f":{"ref":{"kind":"self","id":"factor","output":""}}}`)
	pipe := filepath.Join(dir, "pipeargs")
	writeTestBundle(t, pipe, map[string]any{"vals": []any{10, 20, 30}, "factor": 2})

	// Element for fork 1 (value 20), sliced by the driver.
	elem := writeTestFile(t, filepath.Join(dir, "elem.json"), `20`)
	byElem := filepath.Join(dir, "by_element")

	if err := run(t.Context(), []string{
		"forkbind", "-spec", spec, "-pipeargs", pipe,
		"-elementfile", elem, "-o", byElem,
	}); err != nil {
		t.Fatalf("run forkbind -elementfile: %v", err)
	}

	if diff := cmp.Diff(map[string]any{"v": 20.0, "f": 2.0}, readTestBundle(t, byElem)); diff != "" {
		t.Errorf("element args mismatch (-want +got):\n%s", diff)
	}

	// Byte-identical to the -index write of the same fork.
	byIndex := filepath.Join(dir, "by_index")
	if err := run(t.Context(), []string{
		"forkbind", "-spec", spec, "-pipeargs", pipe,
		"-index", "1", "-o", byIndex,
	}); err != nil {
		t.Fatalf("run forkbind -index: %v", err)
	}

	if diff := cmp.Diff(readTestBundle(t, byIndex), readTestBundle(t, byElem)); diff != "" {
		t.Errorf("-elementfile not byte-identical to -index 1 (-index +element):\n%s", diff)
	}

	// -elementfile without -o is a loud error naming the actual flag pair,
	// not a write to cwd and not a message blaming -index.
	err := run(t.Context(), []string{"forkbind", "-spec", spec, "-pipeargs", pipe, "-elementfile", elem})
	if !errors.Is(err, errElementNoOut) {
		t.Errorf("forkbind -elementfile without -o: err = %v, want errElementNoOut", err)
	}
}

// TestRunForkBindElementMarkerShapedString guards #328 on the native-scatter
// path: the driver slices the element straight out of pipeargs' data.json, so
// a marker-shaped string arrives in its bundle-encoded form and must still
// reach the fork's args as the literal string, the same as the -index write.
func TestRunForkBindElementMarkerShapedString(t *testing.T) {
	const shaped = "@mre:file:f/L0000/x"

	dir := t.TempDir()
	spec := writeTestFile(t, filepath.Join(dir, "spec.json"),
		`{"v":{"ref":{"kind":"self","id":"vals","output":""},"split":true}}`)
	pipe := filepath.Join(dir, "pipeargs")
	writeTestBundle(t, pipe, map[string]any{"vals": []any{"plain", shaped}})

	var sidecar struct {
		Vals []json.RawMessage `json:"vals"`
	}

	readJSONFile(t, filepath.Join(pipe, "data.json"), &sidecar)

	if len(sidecar.Vals) != 2 {
		t.Fatalf("pipeargs vals = %d elements, want 2", len(sidecar.Vals))
	}

	elem := writeTestFile(t, filepath.Join(dir, "elem.json"), string(sidecar.Vals[1]))

	byElem := filepath.Join(dir, "by_element")
	if err := run(t.Context(), []string{
		"forkbind", "-spec", spec, "-pipeargs", pipe, "-elementfile", elem, "-o", byElem,
	}); err != nil {
		t.Fatalf("run forkbind -elementfile: %v", err)
	}

	if diff := cmp.Diff(map[string]any{"v": shaped}, readTestBundle(t, byElem)); diff != "" {
		t.Errorf("element args mismatch (-want +got):\n%s", diff)
	}

	byIndex := filepath.Join(dir, "by_index")
	if err := run(t.Context(), []string{
		"forkbind", "-spec", spec, "-pipeargs", pipe, "-index", "1", "-o", byIndex,
	}); err != nil {
		t.Fatalf("run forkbind -index: %v", err)
	}

	elemData, err := os.ReadFile(filepath.Join(byElem, "data.json"))
	if err != nil {
		t.Fatal(err)
	}

	indexData, err := os.ReadFile(filepath.Join(byIndex, "data.json"))
	if err != nil {
		t.Fatal(err)
	}

	if diff := cmp.Diff(string(indexData), string(elemData)); diff != "" {
		t.Errorf("-elementfile data.json differs from -index 1 (-index +element):\n%s", diff)
	}
}

// TestRunForkBindElementFlagConflicts checks -elementfile rejects the
// full-collection flags it would otherwise silently ignore: the pre-sliced
// element path never indexes, writes keys, or dispatches on -mapmode.
func TestRunForkBindElementFlagConflicts(t *testing.T) {
	dir := t.TempDir()
	spec := writeTestFile(t, filepath.Join(dir, "spec.json"),
		`{"v":{"ref":{"kind":"self","id":"vals","output":""},"split":true}}`)
	pipe := filepath.Join(dir, "pipeargs")
	writeTestBundle(t, pipe, map[string]any{"vals": []any{1}})
	elem := writeTestFile(t, filepath.Join(dir, "elem.json"), `1`)

	cases := [][]string{
		{"-index", "0"},
		{"-keysfile", filepath.Join(dir, "k.json")},
		{"-keysonly"},
		{"-mapmode", "array"},
	}

	for _, extra := range cases {
		argv := append([]string{
			"forkbind", "-spec", spec, "-pipeargs", pipe,
			"-elementfile", elem, "-o", filepath.Join(dir, "out"),
		}, extra...)

		if err := run(t.Context(), argv); !errors.Is(err, errElementConflict) {
			t.Errorf("forkbind -elementfile with %v: err = %v, want errElementConflict", extra, err)
		}
	}
}

// TestRunForkBindNegativeIndex checks that a -index below the -1 "unset" sentinel
// is rejected, not silently folded into the full-collection write — which would
// drop -o and write the whole collection into -chunkdir instead.
func TestRunForkBindNegativeIndex(t *testing.T) {
	dir := t.TempDir()
	spec := writeTestFile(t, filepath.Join(dir, "spec.json"),
		`{"v":{"ref":{"kind":"self","id":"vals","output":""},"split":true}}`)
	pipe := filepath.Join(dir, "pipeargs")
	writeTestBundle(t, pipe, map[string]any{"vals": []any{1}})

	argv := []string{
		"forkbind", "-spec", spec, "-pipeargs", pipe,
		"-index=-2", "-o", filepath.Join(dir, "out"),
	}
	if err := run(t.Context(), argv); !errors.Is(err, errForkIndexNegative) {
		t.Errorf("forkbind -index=-2: err = %v, want errForkIndexNegative", err)
	}
}

// TestRunForkBindElementTypes locks the byte-identity invariant the O(1) path
// depends on across element shapes — a float, a unicode string, and a nested
// struct — since WriteBundle's type coercion is what normalizes the driver's
// re-serialized element to the -index bytes; a regression there would silently
// corrupt fork args.
func TestRunForkBindElementTypes(t *testing.T) {
	cases := []struct {
		name, collection, element string
	}{
		{"float", `[0.1, 42.0]`, `42.0`},
		{"unicode string", `["a", "café🎉"]`, `"café🎉"`},
		{"nested struct", `[{"x":1},{"x":2,"y":"z"}]`, `{"x":2,"y":"z"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			spec := writeTestFile(t, filepath.Join(dir, "spec.json"),
				`{"v":{"ref":{"kind":"self","id":"vals","output":""},"split":true}}`)
			pipe := filepath.Join(dir, "pipeargs")
			writeTestBundle(t, pipe, map[string]any{"vals": json.RawMessage(tc.collection)})

			byElem := filepath.Join(dir, "by_element")
			if err := run(t.Context(), []string{
				"forkbind", "-spec", spec, "-pipeargs", pipe,
				"-elementfile", writeTestFile(t, filepath.Join(dir, "e.json"), tc.element), "-o", byElem,
			}); err != nil {
				t.Fatalf("run -elementfile: %v", err)
			}

			byIndex := filepath.Join(dir, "by_index")
			if err := run(t.Context(), []string{
				"forkbind", "-spec", spec, "-pipeargs", pipe,
				"-index", "1", "-o", byIndex,
			}); err != nil {
				t.Fatalf("run -index: %v", err)
			}

			if diff := cmp.Diff(readTestBundle(t, byIndex), readTestBundle(t, byElem)); diff != "" {
				t.Errorf("-elementfile not byte-identical to -index 1 (-index +element):\n%s", diff)
			}
		})
	}
}

// TestRunForkBindElementMap drives the O(1) element path for a MAP fork: a
// pre-sliced map VALUE assembles byte-identically to the -index write of that
// key's fork, so the driver's per-key element slicing matches ResolveForks.
func TestRunForkBindElementMap(t *testing.T) {
	dir := t.TempDir()
	spec := writeTestFile(t, filepath.Join(dir, "spec.json"),
		`{"v":{"ref":{"kind":"self","id":"m","output":""},"split":true}}`)
	pipe := filepath.Join(dir, "pipeargs")
	// Sorted keys a,b,c → fork indices 0,1,2; fork 1 is key "b" → value 20.
	writeTestBundle(t, pipe, map[string]any{"m": map[string]any{"a": 10, "b": 20, "c": 30}})

	elem := writeTestFile(t, filepath.Join(dir, "elem.json"), `20`)
	byElem := filepath.Join(dir, "by_element")

	// The element path is fork-kind-agnostic (no -mapmode): the driver already
	// sliced the map value, so the same command serves array and map forks.
	if err := run(t.Context(), []string{
		"forkbind", "-spec", spec, "-pipeargs", pipe,
		"-elementfile", elem, "-o", byElem,
	}); err != nil {
		t.Fatalf("run forkbind -elementfile (map): %v", err)
	}

	byIndex := filepath.Join(dir, "by_index")
	if err := run(t.Context(), []string{
		"forkbind", "-spec", spec, "-pipeargs", pipe,
		"-mapmode", "map", "-index", "1", "-o", byIndex,
	}); err != nil {
		t.Fatalf("run forkbind -index (map): %v", err)
	}

	if diff := cmp.Diff(readTestBundle(t, byIndex), readTestBundle(t, byElem)); diff != "" {
		t.Errorf("map -elementfile not byte-identical to -index 1 (-index +element):\n%s", diff)
	}
}

// TestRunForkBindIndexKeysFile drives the -keysfile side of the native scatter:
// each -index instance can emit the forkmeta sidecar byte-identical to the full
// write's (map keys for a map fork, null for an array fork), so the gather gets
// its keys without a FORK task. -keysfile without -index is a loud error.
func TestRunForkBindIndexKeysFile(t *testing.T) {
	tests := []struct {
		name, spec, mapMode, want string
	}{
		{name: "array fork", spec: `{"n":{"literal":[10,20],"split":true}}`, mapMode: "array", want: `{"count":2,"keys":null}`},
		{name: "map fork", spec: `{"n":{"literal":{"a":1,"b":2},"split":true}}`, mapMode: "map", want: `{"count":2,"keys":["a","b"]}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			spec := writeTestFile(t, filepath.Join(dir, "spec.json"), tc.spec)
			keys := filepath.Join(dir, "forkmeta.json")

			if err := run(t.Context(), []string{
				"forkbind", "-spec", spec, "-mapmode", tc.mapMode,
				"-index", "0", "-o", filepath.Join(dir, "args"), "-keysfile", keys,
			}); err != nil {
				t.Fatalf("run forkbind -index -keysfile: %v", err)
			}

			got, err := os.ReadFile(keys)
			if err != nil {
				t.Fatalf("read keysfile: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("keysfile = %q, want %q", got, tc.want)
			}

			// Byte-identical to the full-fork write's forkmeta.json.
			forks := filepath.Join(dir, "forks")
			if err := run(t.Context(), []string{
				"forkbind", "-spec", spec, "-mapmode", tc.mapMode,
				"-chunkdir", forks,
			}); err != nil {
				t.Fatalf("run default forkbind: %v", err)
			}

			full, err := os.ReadFile(filepath.Join(forks, "forkmeta.json"))
			if err != nil {
				t.Fatalf("read full-write forkmeta.json: %v", err)
			}
			if string(got) != string(full) {
				t.Errorf("keysfile %q not byte-identical to full write %q", got, full)
			}
		})
	}

	dir := t.TempDir()
	spec := writeTestFile(t, filepath.Join(dir, "spec.json"), `{"n":{"literal":[1],"split":true}}`)

	// The guard must fire as the SPECIFIC flag error, before any resolution work
	// (a wrapped I/O error here would mean the guard regressed to a side effect).
	err := run(t.Context(), []string{"forkbind", "-spec", spec, "-keysfile", filepath.Join(dir, "k.json")})
	if !errors.Is(err, errKeysFileNeedsIndex) {
		t.Errorf("forkbind -keysfile without -index: err = %v, want errKeysFileNeedsIndex", err)
	}
}

// TestRunForkBindKeysOnly drives the native scatter's empty-collection
// sentinel (#76): -keysonly resolves and VALIDATES every binding like the FORK
// task would — a wrong-kind split source still fails loudly — and writes only
// the keys sidecar (no fork bundle).
func TestRunForkBindKeysOnly(t *testing.T) {
	tests := []struct {
		name, spec, mapMode, want string
	}{
		{name: "empty array fork", spec: `{"n":{"literal":[],"split":true}}`, mapMode: "array", want: `{"count":0,"keys":null}`},
		{name: "empty map fork", spec: `{"n":{"literal":{},"split":true}}`, mapMode: "map", want: `{"count":0,"keys":[]}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			spec := writeTestFile(t, filepath.Join(dir, "spec.json"), tc.spec)
			keys := filepath.Join(dir, "forkmeta.json")

			if err := run(t.Context(), []string{
				"forkbind", "-spec", spec, "-mapmode", tc.mapMode,
				"-keysonly", "-keysfile", keys,
			}); err != nil {
				t.Fatalf("run forkbind -keysonly: %v", err)
			}

			got, err := os.ReadFile(keys)
			if err != nil {
				t.Fatalf("read keysfile: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("keysfile = %q, want %q", got, tc.want)
			}

			if _, err := os.Stat(filepath.Join(dir, "fork_00000")); !os.IsNotExist(err) {
				t.Error("-keysonly must not write fork bundles")
			}
		})
	}

	dir := t.TempDir()

	// Validation parity with the FORK task: an array split value under map mode
	// must fail loudly even though it would scatter zero forks.
	bad := writeTestFile(t, filepath.Join(dir, "bad.json"), `{"n":{"literal":[],"split":true}}`)
	err := run(t.Context(), []string{
		"forkbind", "-spec", bad, "-mapmode", "map",
		"-keysonly", "-keysfile", filepath.Join(dir, "k.json"),
	})
	if err == nil || !strings.Contains(err.Error(), "not a map") {
		t.Errorf("-keysonly on [] under map mode: err = %v, want the FORK task's not-a-map error", err)
	}

	if err := run(t.Context(), []string{"forkbind", "-spec", bad, "-keysonly"}); !errors.Is(err, errKeysOnlyNeedsFile) {
		t.Errorf("forkbind -keysonly without -keysfile: err = %v, want errKeysOnlyNeedsFile", err)
	}

	// -keysonly with -index would silently skip the -o bundle write: loud error.
	if err := run(t.Context(), []string{
		"forkbind", "-spec", bad, "-keysonly", "-index", "0",
		"-o", filepath.Join(dir, "a"), "-keysfile", filepath.Join(dir, "k2.json"),
	}); !errors.Is(err, errKeysOnlyWithIndex) {
		t.Errorf("forkbind -keysonly with -index: err = %v, want errKeysOnlyWithIndex", err)
	}
}

// TestRunForkBindKeysOnlyPruned pins #436: forkbind marks a split of a
// disabled call's bundle, or of a field a bundle records as statically null,
// pruned; a split of a null that ran is not.
func TestRunForkBindKeysOnlyPruned(t *testing.T) {
	for _, tc := range []struct {
		name, spec string
		upNull     bool
		recorded   bool
		want       string
	}{
		{name: "split of a disabled call", spec: upSplit, upNull: true, want: `{"count":0,"keys":null,"pruned":true}`},
		{name: "split of a null output", spec: upSplit, want: `{"count":0,"keys":null}`},
		{name: "split of a recorded output", spec: upSplit, recorded: true, want: `{"count":0,"keys":null,"pruned":true}`},
		{name: "split of a recorded args field", spec: selfSplit, recorded: true, want: `{"count":0,"keys":null,"pruned":true}`},
		{name: "split of a null args field", spec: selfSplit, want: `{"count":0,"keys":null}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			up, pa := filepath.Join(dir, "up"), filepath.Join(dir, "pa")

			if tc.upNull {
				if err := shim.WriteNullBundle(up); err != nil {
					t.Fatal(err)
				}
			} else {
				writeTestBundle(t, up, map[string]any{"xs": nil})
			}

			writeTestBundle(t, pa, map[string]any{"xs": nil})

			if tc.recorded {
				for _, b := range []string{up, pa} {
					if err := writeStaticNulls(b, []string{"xs"}); err != nil {
						t.Fatal(err)
					}
				}
			}

			spec := writeTestFile(t, filepath.Join(dir, "spec.json"), tc.spec)
			keys := filepath.Join(dir, "forkmeta.json")

			if err := run(t.Context(), []string{
				"forkbind", "-spec", spec, "-pipeargs", pa, "-inputs", "UP=" + up, "-keysonly", "-keysfile", keys,
			}); err != nil {
				t.Fatalf("run forkbind -keysonly: %v", err)
			}

			got, err := os.ReadFile(keys)
			if err != nil {
				t.Fatalf("read keysfile: %v", err)
			}

			if string(got) != tc.want {
				t.Errorf("keysfile = %q, want %q", got, tc.want)
			}
		})
	}
}

// The split specs of TestRunForkBindKeysOnlyPruned's cases.
const (
	upSplit   = `{"n":{"ref":{"kind":"call","id":"UP","output":"xs"},"split":true}}`
	selfSplit = `{"n":{"ref":{"kind":"self","id":"xs"},"split":true}}`
)

// TestRunBindRecordsStaticNulls pins the #436 sidecar a bind writes: the
// params bound to a disabled call's output, and through a bundle that records
// a static null, are listed; a bind with none writes no sidecar.
func TestRunBindRecordsStaticNulls(t *testing.T) {
	dir := t.TempDir()
	off, ran, pa := filepath.Join(dir, "off"), filepath.Join(dir, "ran"), filepath.Join(dir, "pa")

	if err := shim.WriteNullBundle(off); err != nil {
		t.Fatal(err)
	}

	writeTestBundle(t, ran, map[string]any{"xs": nil})
	writeTestBundle(t, pa, map[string]any{"gone": nil, "kept": nil})
	if err := writeStaticNulls(pa, []string{"gone"}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, spec string
		want       []string
	}{
		{
			name: "static nulls",
			spec: `{"v":{"ref":{"kind":"call","id":"OFF","output":"xs"}},"g":{"ref":{"kind":"self","id":"gone"}},` +
				`"r":{"ref":{"kind":"call","id":"RAN","output":"xs"}},"k":{"ref":{"kind":"self","id":"kept"}}}`,
			want: []string{"g", "v"},
		},
		{name: "none", spec: `{"r":{"ref":{"kind":"call","id":"RAN","output":"xs"}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "args")
			spec := writeTestFile(t, filepath.Join(t.TempDir(), "spec.json"), tc.spec)

			if err := run(t.Context(), []string{
				"bind", "-spec", spec, "-pipeargs", pa, "-inputs", "OFF=" + off + ",RAN=" + ran, "-o", out,
			}); err != nil {
				t.Fatalf("run bind: %v", err)
			}

			got, err := readNullFields(out)
			if err != nil {
				t.Fatal(err)
			}

			if diff := cmp.Diff(tc.want, slices.Sorted(maps.Keys(got))); diff != "" {
				t.Errorf("recorded static nulls mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestRunGate drives `mre gate` end to end: it writes the gate as a bundle
// whose off field reads the flag, here a pruned zero-fork split.
func TestRunGate(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags []string
		want  bool
	}{
		{"runtime empty", nil, false},
		{"known empty", []string{"-emptynull"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			out := filepath.Join(dir, "gate")
			spec := writeTestFile(t, filepath.Join(dir, "spec.json"), `{"v":{"literal":[],"split":true}}`)

			if err := run(t.Context(), append([]string{"gate", "-spec", spec, "-o", out}, tc.flags...)); err != nil {
				t.Fatalf("run gate: %v", err)
			}

			if diff := cmp.Diff(map[string]any{"off": tc.want}, readTestBundle(t, out)); diff != "" {
				t.Errorf("gate bundle mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestRunMergeNullForks pins #434's `merge -nullforks`: it gathers nothing
// and gives one null per fork the sidecar counts, the typed empty for zero
// forks, and null for zero forks under -emptynull.
func TestRunMergeNullForks(t *testing.T) {
	for _, tc := range []struct {
		name, keysJSON string
		flags          []string
		want           any
	}{
		{"array forks", `{"count":2,"keys":null}`, nil, map[string]any{"@nullforks": []any{nil, nil}, "s": []any{nil, nil}}},
		{"map forks", `{"count":1,"keys":["k"]}`, nil, map[string]any{"@nullforks": map[string]any{"k": nil}, "s": map[string]any{"k": nil}}},
		{"zero forks", `{"count":0,"keys":null}`, nil, map[string]any{"@nullforks": []any{}, "s": []any{}}},
		{"zero known-empty forks", `{"count":0,"keys":null}`, []string{"-emptynull"}, map[string]any{"s": nil}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			out := filepath.Join(dir, "merged")
			argv := append([]string{
				"merge", "-nullforks", "-outs", "s", "-forksdir", dir, "-o", out,
				"-keysfile", writeTestFile(t, filepath.Join(dir, "keys.json"), tc.keysJSON),
			}, tc.flags...)

			if err := run(t.Context(), argv); err != nil {
				t.Fatalf("run merge -nullforks: %v", err)
			}

			if diff := cmp.Diff(tc.want, any(readTestBundle(t, out))); diff != "" {
				t.Errorf("merged outs mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestRunMergeRecordsPrune pins that a zero-fork merge to null records every
// output as statically null, and a typed-empty merge records none.
func TestRunMergeRecordsPrune(t *testing.T) {
	for _, tc := range []struct {
		name, keysJSON string
		want           []string
	}{
		{name: "pruned", keysJSON: `{"count":0,"keys":null,"pruned":true}`, want: []string{"a", "b"}},
		{name: "runtime empty", keysJSON: `{"count":0,"keys":null}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			out := filepath.Join(dir, "merged")

			if err := run(t.Context(), []string{
				"merge", "-outs", "a,b", "-forksdir", filepath.Join(dir, "souts"), "-o", out,
				"-keysfile", writeTestFile(t, filepath.Join(dir, "keys.json"), tc.keysJSON),
			}); err != nil {
				t.Fatalf("run merge: %v", err)
			}

			got, err := readNullFields(out)
			if err != nil {
				t.Fatal(err)
			}

			if diff := cmp.Diff(tc.want, slices.Sorted(maps.Keys(got))); diff != "" {
				t.Errorf("recorded static nulls mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestRunMergeSmoke drives merge end to end from a staged forks dir: an array
// fork collects each output into an array in fork-index order; a keyed (map)
// fork rebuilds a map from the keys sidecar; zero forks (no staged dir at all,
// as Nextflow stages an empty list) merge to the typed empty.
func TestRunMergeSmoke(t *testing.T) {
	tests := []struct {
		name     string
		forks    []string // outs__ key per fork, written in this (unsorted) order
		keysJSON string
		want     map[string]any
	}{
		{
			name:     "array fork",
			forks:    []string{"fork_00001", "fork_00000"},
			keysJSON: `{"count":2,"keys":null}`,
			want:     map[string]any{"s": []any{0.0, 1.0}},
		},
		{
			name:     "map fork",
			forks:    []string{"fork_00000", "fork_00001"},
			keysJSON: `{"count":2,"keys":["a","b"]}`,
			want:     map[string]any{"s": map[string]any{"a": 0.0, "b": 1.0}},
		},
		{
			name:     "composite keys",
			forks:    []string{"fork_00003~fork_00001", "fork_00003~fork_00000"},
			keysJSON: `{"count":2,"keys":null}`,
			want:     map[string]any{"s": []any{0.0, 1.0}},
		},
		{name: "zero array forks", keysJSON: `{"count":0,"keys":null}`, want: map[string]any{"s": []any{}}},
		{name: "zero map forks", keysJSON: `{"count":0,"keys":[]}`, want: map[string]any{"s": map[string]any{}}},
		// #436: a split of a disabled call's output is pruned to null.
		{name: "zero pruned forks", keysJSON: `{"count":0,"keys":null,"pruned":true}`, want: map[string]any{"s": nil}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			forks := filepath.Join(dir, "souts")
			writeForkOuts(t, forks, tc.forks)
			out := filepath.Join(dir, "merged")

			argv := []string{
				"merge", "-outs", "s", "-forksdir", forks, "-o", out,
				"-keysfile", writeTestFile(t, filepath.Join(dir, "keys.json"), tc.keysJSON),
			}
			if err := run(t.Context(), argv); err != nil {
				t.Fatalf("run merge: %v", err)
			}

			if diff := cmp.Diff(tc.want, readTestBundle(t, out)); diff != "" {
				t.Errorf("merged outs mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestRunMergeCountMismatch is the #324 guard: a merge whose staged fork
// bundles do not number the sidecar's fork count fails loudly instead of
// publishing a short (or empty) result, and the sidecar itself must be
// present and self-consistent.
func TestRunMergeCountMismatch(t *testing.T) {
	tests := []struct {
		name     string
		forks    []string
		keysJSON string // empty = no -keysfile
		wantErr  error
	}{
		{name: "fork missing", forks: []string{"fork_00000"}, keysJSON: `{"count":2,"keys":null}`, wantErr: errGatherCount},
		{name: "no forks staged", keysJSON: `{"count":3,"keys":null}`, wantErr: errGatherCount},
		{name: "extra fork", forks: []string{"fork_00000", "fork_00001"}, keysJSON: `{"count":1,"keys":null}`, wantErr: errGatherCount},
		{name: "index gap", forks: []string{"fork_00000", "fork_00002"}, keysJSON: `{"count":2,"keys":null}`, wantErr: errGatherEntry},
		{name: "keys disagree with count", forks: []string{"fork_00000"}, keysJSON: `{"count":1,"keys":["a","b"]}`, wantErr: errForkMeta},
		{name: "sidecar without count", forks: []string{"fork_00000"}, keysJSON: `{"keys":null}`, wantErr: errForkMeta},
		{name: "no keysfile", forks: []string{"fork_00000"}, wantErr: errNoKeysFile},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			forks := filepath.Join(dir, "souts")
			writeForkOuts(t, forks, tc.forks)

			argv := []string{"merge", "-outs", "s", "-forksdir", forks, "-o", filepath.Join(dir, "merged")}
			if tc.keysJSON != "" {
				argv = append(argv, "-keysfile", writeTestFile(t, filepath.Join(dir, "keys.json"), tc.keysJSON))
			}

			if err := run(t.Context(), argv); !errors.Is(err, tc.wantErr) {
				t.Errorf("run merge: err = %v, want %v", err, tc.wantErr)
			}

			if _, err := os.Stat(filepath.Join(dir, "merged")); !os.IsNotExist(err) {
				t.Error("a failed merge must not write the merged bundle")
			}
		})
	}
}

// writeForkOuts stages one outs__<key> bundle per key under dir, each carrying
// its key's trailing fork index as output "s" so a test can read back the order.
func writeForkOuts(t *testing.T, dir string, keys []string) {
	t.Helper()

	for _, key := range keys {
		_, idx, err := splitIndex(key)
		if err != nil {
			t.Fatal(err)
		}

		writeTestBundle(t, filepath.Join(dir, forkOutPrefix+key), map[string]any{"s": idx})
	}
}

// TestRunPublishLayoutSmoke drives publish-layout end to end from a raw
// marker-bearing sidecar: layout.json maps the transport basename to its outs/
// rel path, pipeline_outs.json carries the rewritten value tree, and
// manifest.json.gz gunzips to the versioned output index.
func TestRunPublishLayoutSmoke(t *testing.T) {
	dir := t.TempDir()
	man := types.Manifest{Callables: map[string]types.Callable{
		"PIPE": {Out: []ir.Param{{Name: "aln", BaseType: "bam", IsFile: true}}},
	}}
	manPath := filepath.Join(dir, "types.json")
	if err := man.Write(manPath); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	sidecar := writeTestFile(t, filepath.Join(dir, "data.json"),
		`{"aln":"`+marker("L0000")+`"}`)
	outDir := filepath.Join(dir, "outs")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("mkdir outs: %v", err)
	}

	argv := []string{"publish-layout", "-types", manPath, "-callable", "PIPE", "-sidecar", sidecar, "-dir", outDir}
	if err := run(t.Context(), argv); err != nil {
		t.Fatalf("run publish-layout: %v", err)
	}

	var layout map[string][]string

	readJSONFile(t, filepath.Join(outDir, "layout.json"), &layout)

	if diff := cmp.Diff(map[string][]string{leaf("L0000"): {"aln.bam"}}, layout); diff != "" {
		t.Errorf("layout.json mismatch (-want +got):\n%s", diff)
	}

	var outs map[string]any

	readJSONFile(t, filepath.Join(outDir, "pipeline_outs.json"), &outs)

	if diff := cmp.Diff(map[string]any{"aln": "aln.bam"}, outs); diff != "" {
		t.Errorf("pipeline_outs.json mismatch (-want +got):\n%s", diff)
	}

	gz, err := os.Open(filepath.Join(outDir, "manifest.json.gz"))
	if err != nil {
		t.Fatalf("open manifest.json.gz: %v", err)
	}
	defer func() { _ = gz.Close() }()

	zr, err := gzip.NewReader(gz)
	if err != nil {
		t.Fatalf("gunzip manifest: %v", err)
	}

	data, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}

	var index struct {
		SchemaVersion int             `json:"schema_version"`
		Pipeline      string          `json:"pipeline"`
		Outputs       []manifestEntry `json:"outputs"`
	}
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}

	if index.SchemaVersion != 1 || index.Pipeline != "PIPE" {
		t.Errorf("manifest header = %d/%q, want 1/PIPE", index.SchemaVersion, index.Pipeline)
	}

	wantOutputs := []manifestEntry{{Path: "aln.bam", BaseType: "bam", IsDir: false}}
	if diff := cmp.Diff(wantOutputs, index.Outputs); diff != "" {
		t.Errorf("manifest outputs mismatch (-want +got):\n%s", diff)
	}
}

// TestRunEntryArgsSmoke drives entryargs end to end: a supplied run-parameter
// value overrides the baked default; a null value keeps it.
func TestRunEntryArgsSmoke(t *testing.T) {
	tests := []struct {
		name   string
		values string
		want   float64
	}{
		{name: "override replaces default", values: `{"n":5}`, want: 5},
		{name: "null keeps default", values: `{"n":null}`, want: 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			base := filepath.Join(dir, "base")
			writeTestBundle(t, base, map[string]any{"n": 1})
			values := writeTestFile(t, filepath.Join(dir, "values.json"), tc.values)
			out := filepath.Join(dir, "out")

			if err := run(t.Context(), []string{"entryargs", "-base", base, "-values", values, "-o", out}); err != nil {
				t.Fatalf("run entryargs: %v", err)
			}

			if diff := cmp.Diff(map[string]any{"n": tc.want}, readTestBundle(t, out)); diff != "" {
				t.Errorf("entry args mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
