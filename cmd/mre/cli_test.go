package main

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/eunmann/mro2nf/internal/types"
	"github.com/google/go-cmp/cmp"
)

// writeTestBundle writes payload as a bundle directory with no file leaves.
func writeTestBundle(t *testing.T, dir string, payload map[string]any) {
	t.Helper()

	if err := shim.WriteBundle(dir, payload, nil, types.NewTable(nil)); err != nil {
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

// TestForkMetaRoundTrip round-trips the fork sidecars: an array fork writes a
// literal JSON null to forkkeys.json (read back as nil keys), a map fork writes
// its key array; forknames.json always lists the fork bundle dirs.
func TestForkMetaRoundTrip(t *testing.T) {
	names := []string{"fork_00000", "fork_00001"}

	tests := []struct {
		name     string
		keys     []string
		wantFile string
	}{
		{name: "array fork", keys: nil, wantFile: "null"},
		{name: "map fork", keys: []string{"a", "b"}, wantFile: `["a","b"]`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := writeForkMeta(dir, names, tc.keys); err != nil {
				t.Fatalf("writeForkMeta: %v", err)
			}

			keysPath := filepath.Join(dir, "forkkeys.json")

			data, err := os.ReadFile(keysPath)
			if err != nil {
				t.Fatalf("read forkkeys.json: %v", err)
			}

			if string(data) != tc.wantFile {
				t.Errorf("forkkeys.json = %q, want %q", data, tc.wantFile)
			}

			gotKeys, err := readForkKeys(keysPath)
			if err != nil {
				t.Fatalf("readForkKeys: %v", err)
			}

			if diff := cmp.Diff(tc.keys, gotKeys); diff != "" {
				t.Errorf("keys round-trip mismatch (-want +got):\n%s", diff)
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

	if params := man.Params("ANY", types.RoleIn); params != nil {
		t.Errorf("empty manifest Params = %v, want nil", params)
	}
}

// TestProducerCoerceInputs checks the int-from-whole-float coercion against a
// manifest written to disk, concatenating the RoleIn and RoleChunkIn parameter
// sets; a non-int value passes through untouched.
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

	raw, err := prod.coerceInputs(json.RawMessage(`{"x":5.0,"c":7.0,"other":1.5}`), types.RoleIn, types.RoleChunkIn)
	if err != nil {
		t.Fatalf("coerceInputs: %v", err)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode coerced args: %v", err)
	}

	want := map[string]json.RawMessage{
		"x":     json.RawMessage("5"),
		"c":     json.RawMessage("7"),
		"other": json.RawMessage("1.5"),
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("coerced args mismatch (-want +got):\n%s", diff)
	}
}

// TestProducerCoerceChunk checks that a whole-float per-chunk arg bound to an
// int ChunkIn param is coerced to an integer.
func TestProducerCoerceChunk(t *testing.T) {
	man := types.Manifest{Callables: map[string]types.Callable{
		"S": {ChunkIn: []ir.Param{{Name: "c", Type: "int", BaseType: "int"}}},
	}}
	path := filepath.Join(t.TempDir(), "types.json")
	if err := man.Write(path); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	prod := &producer{types: path, callable: "S"}
	chunk := shim.ChunkDef{Args: map[string]json.RawMessage{"c": json.RawMessage("5.0")}}

	got, err := prod.coerceChunk(chunk)
	if err != nil {
		t.Fatalf("coerceChunk: %v", err)
	}

	if diff := cmp.Diff(map[string]json.RawMessage{"c": json.RawMessage("5")}, got.Args); diff != "" {
		t.Errorf("coerced chunk args mismatch (-want +got):\n%s", diff)
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

// TestRunForkBindArraySmoke drives forkbind in array mode over a literal split:
// one fork bundle per element, forknames.json listing them in order, and a
// literal null forkkeys.json (no map keys).
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

	keysRaw, err := os.ReadFile(filepath.Join(forks, "forkkeys.json"))
	if err != nil {
		t.Fatalf("read forkkeys.json: %v", err)
	}

	if string(keysRaw) != "null" {
		t.Errorf("forkkeys.json = %q, want null", keysRaw)
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

// TestRunMergeSmoke drives merge end to end: an array fork collects each output
// into an ordered array; a keyed (map) fork rebuilds a map from the keys file.
func TestRunMergeSmoke(t *testing.T) {
	tests := []struct {
		name     string
		keysJSON string // empty = array fork (no -keys-file)
		want     map[string]any
	}{
		{name: "array fork", want: map[string]any{"s": []any{1.0, 2.0}}},
		{name: "map fork", keysJSON: `["a","b"]`, want: map[string]any{"s": map[string]any{"a": 1.0, "b": 2.0}}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			fork0 := filepath.Join(dir, "f0")
			fork1 := filepath.Join(dir, "f1")
			writeTestBundle(t, fork0, map[string]any{"s": 1})
			writeTestBundle(t, fork1, map[string]any{"s": 2})
			out := filepath.Join(dir, "merged")

			argv := []string{"merge", "-outs", "s", "-files", fork0 + "," + fork1, "-o", out}
			if tc.keysJSON != "" {
				argv = append(argv, "-keys-file", writeTestFile(t, filepath.Join(dir, "keys.json"), tc.keysJSON))
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

	if diff := cmp.Diff(map[string][]string{"L0000": {"aln.bam"}}, layout); diff != "" {
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
