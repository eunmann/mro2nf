package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/types"
)

// keyOrderManifest declares In(z, a), Outer(inner In, b) and a file-bearing
// FOut(q In, f file, b), whose declared, sorted and test-producer orders all
// differ, with S taking and returning an Outer, CONS taking an Outer bound two
// ways, and PIPE returning one plain and one file-bearing struct.
func keyOrderManifest(t *testing.T, dir string) string {
	t.Helper()

	man := types.Manifest{
		Structs: map[string]*ir.StructType{
			"In": {Name: "In", Fields: []ir.Param{{Name: "z", BaseType: "int"}, {Name: "a", BaseType: "int"}}},
			"Outer": {Name: "Outer", Fields: []ir.Param{
				{Name: "inner", BaseType: "In"}, {Name: "b", BaseType: "int"},
			}},
			"FOut": {Name: "FOut", Fields: []ir.Param{
				{Name: "q", BaseType: "In"}, {Name: "f", BaseType: "file", IsFile: true}, {Name: "b", BaseType: "int"},
			}},
		},
		Callables: map[string]types.Callable{
			"S":    {In: []ir.Param{{Name: "o", BaseType: "Outer"}}, Out: []ir.Param{{Name: "o", BaseType: "Outer"}}},
			"CONS": {In: []ir.Param{{Name: "o", BaseType: "Outer"}, {Name: "built", BaseType: "Outer"}}},
			"PIPE": {Out: []ir.Param{
				{Name: "st", BaseType: "Outer"}, {Name: "fs", BaseType: "FOut", IsFile: true},
			}},
		},
	}

	return writeTestManifest(t, filepath.Join(dir, "types.json"), man)
}

func compactData(t *testing.T, path string) string {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatalf("compact %s: %v", path, err)
	}

	return buf.String()
}

// writeRawBundle writes data as a bundle's data.json verbatim, as a stage's
// own key order reaches it.
func writeRawBundle(t *testing.T, dir, data string) {
	t.Helper()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	writeTestFile(t, filepath.Join(dir, "data.json"), data)
}

// TestRunBindKeepsKeyOrder checks bind hands a consumer the key order mrp
// does (nested_struct's READN, #366): a projected struct as its producer
// wrote it, and a struct built from bindings with its keys sorted and its
// member filtered into declared order.
func TestRunBindKeepsKeyOrder(t *testing.T) {
	dir := t.TempDir()
	spec := writeTestFile(t, filepath.Join(dir, "spec.json"), `{
		"o": {"ref": {"kind": "call", "id": "UP", "output": "o"}},
		"built": {"object": {"inner": {"ref": {"kind": "call", "id": "UP", "output": "s"}}, "b": {"literal": 4}}}
	}`)
	up := filepath.Join(dir, "in_UP")
	writeRawBundle(t, up, `{"o":{"inner":{"a":2,"z":1},"b":3},"s":{"a":2,"m":9,"z":1}}`)
	out := filepath.Join(dir, "out")

	if err := run(t.Context(), []string{
		"bind", "-spec", spec, "-inputs", "UP=" + up, "-o", out,
		"-types", keyOrderManifest(t, dir), "-callable", "CONS", "-role", types.RoleIn,
	}); err != nil {
		t.Fatalf("run bind: %v", err)
	}

	want := `{"built":{"b":4,"inner":{"z":1,"a":2}},"o":{"inner":{"a":2,"z":1},"b":3}}`
	if got := compactData(t, filepath.Join(out, "data.json")); got != want {
		t.Errorf("bound args = %s, want %s", got, want)
	}
}

// TestProducerKeepsStageKeyOrder checks a stage's _outs reach its output
// bundle, and its _args its coerced inputs, with nested objects in the order
// the stage and its producer wrote them.
func TestProducerKeepsStageKeyOrder(t *testing.T) {
	dir := t.TempDir()
	manPath := keyOrderManifest(t, dir)

	out := filepath.Join(dir, "out")
	prod := &producer{types: manPath, callable: "S", role: types.RoleOut}

	if err := prod.write(out, json.RawMessage(`{"o":{"inner":{"a":2,"z":1},"b":3},"extra":{"y":1,"x":2}}`)); err != nil {
		t.Fatalf("write: %v", err)
	}

	wantOuts := `{"extra":{"y":1,"x":2},"o":{"inner":{"a":2,"z":1},"b":3}}`
	if got := compactData(t, filepath.Join(out, "data.json")); got != wantOuts {
		t.Errorf("outs bundle = %s, want %s", got, wantOuts)
	}

	in := &producer{types: manPath, callable: "S"}

	coerced, err := in.coerceInputs(json.RawMessage(`{"o":{"inner":{"z":1.0,"a":2},"b":3.0}}`))
	if err != nil {
		t.Fatalf("coerceInputs: %v", err)
	}

	if got, want := string(coerced), `{"o":{"inner":{"z":1,"a":2},"b":3}}`; got != want {
		t.Errorf("coerced args = %s, want %s", got, want)
	}
}

// TestRunForkBindElementKeepsKeyOrder checks a pre-sliced fork element reaches
// the fork's args in the order it was sliced from its bundle.
func TestRunForkBindElementKeepsKeyOrder(t *testing.T) {
	dir := t.TempDir()
	spec := writeTestFile(t, filepath.Join(dir, "spec.json"), `{"o":{"ref":{"kind":"self","id":"os","output":""},"split":true}}`)
	pipe := filepath.Join(dir, "pipeargs")
	writeRawBundle(t, pipe, `{"os":[{"inner":{"a":2,"z":1},"b":3}]}`)
	elem := writeTestFile(t, filepath.Join(dir, "e.json"), `{"inner":{"a":2,"z":1},"b":3}`)
	byElem := filepath.Join(dir, "by_element")
	byIndex := filepath.Join(dir, "by_index")
	typesFlag := []string{"-types", keyOrderManifest(t, dir), "-callable", "S", "-role", types.RoleIn}

	for _, argv := range [][]string{{"-elementfile", elem, "-o", byElem}, {"-index", "0", "-o", byIndex}} {
		base := []string{"forkbind", "-spec", spec, "-pipeargs", pipe}
		if err := run(t.Context(), append(append(base, argv...), typesFlag...)); err != nil {
			t.Fatalf("run forkbind %v: %v", argv, err)
		}
	}

	want := `{"o":{"inner":{"a":2,"z":1},"b":3}}`
	for _, dir := range []string{byElem, byIndex} {
		if got := compactData(t, filepath.Join(dir, "data.json")); got != want {
			t.Errorf("%s args = %s, want %s", filepath.Base(dir), got, want)
		}
	}
}

// TestRunForkBindLiteralElementsKeepKeyOrder checks each fork of a split
// array or map literal is narrowed by its own element's binding, on the full
// write and on -index: a struct literal keeps its sorted keys with its
// projected member filtered into declared order, and a whole-struct ref is
// filtered into declared order.
func TestRunForkBindLiteralElementsKeepKeyOrder(t *testing.T) {
	const (
		built = `{"object":{"inner":{"ref":{"kind":"call","id":"UP","output":"w"}},"b":{"literal":1}}}`
		whole = `{"ref":{"kind":"call","id":"UP","output":"o"}}`
	)

	cases := []struct{ name, mapMode, split string }{
		{"array", "array", `{"array":[` + built + `,` + whole + `],"split":true}`},
		{"map", "map", `{"object":{"k1":` + built + `,"k2":` + whole + `},"split":true}`},
	}
	want := []string{`{"o":{"b":1,"inner":{"z":1,"a":2}}}`, `{"o":{"inner":{"a":2,"z":1},"b":3}}`}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			spec := writeTestFile(t, filepath.Join(dir, "spec.json"), `{"o":`+tc.split+`}`)
			up := filepath.Join(dir, "in_UP")
			writeRawBundle(t, up, `{"w":{"a":2,"m":9,"z":1},"o":{"b":3,"x":0,"inner":{"a":2,"z":1}}}`)
			base := []string{
				"forkbind", "-spec", spec, "-inputs", "UP=" + up, "-mapmode", tc.mapMode,
				"-types", keyOrderManifest(t, dir), "-callable", "S", "-role", types.RoleIn,
			}
			forks := filepath.Join(dir, "forks")
			byIndex0, byIndex1 := filepath.Join(dir, "by_index_0"), filepath.Join(dir, "by_index_1")
			got := []string{filepath.Join(forks, "fork_00000"), filepath.Join(forks, "fork_00001"), byIndex0, byIndex1}

			for _, argv := range [][]string{
				{"-chunkdir", forks}, {"-index", "0", "-o", byIndex0}, {"-index", "1", "-o", byIndex1},
			} {
				if err := run(t.Context(), append(slices.Clone(base), argv...)); err != nil {
					t.Fatalf("run forkbind %v: %v", argv, err)
				}
			}

			for i, path := range got {
				if data := compactData(t, filepath.Join(path, "data.json")); data != want[i%2] {
					t.Errorf("%s args = %s, want %s", path, data, want[i%2])
				}
			}
		})
	}
}

// TestRunPublishLayoutKeepsKeyOrder checks pipeline_outs.json keeps a plain
// struct output in its producer's order (kitchen_sink's stats, #366), while a
// file-bearing struct takes sorted keys, as mrp's moveOutDir writes it, with
// its plain member as produced.
func TestRunPublishLayoutKeepsKeyOrder(t *testing.T) {
	dir := t.TempDir()
	sidecar := writeTestFile(t, filepath.Join(dir, "data.json"),
		`{"fs":{"q":{"a":2,"z":1},"f":"`+marker("L0000")+`","b":3},"st":{"inner":{"a":2,"z":1},"b":3}}`)
	outDir := filepath.Join(dir, "outs")

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}

	argv := []string{
		"publish-layout", "-types", keyOrderManifest(t, dir), "-callable", "PIPE",
		"-sidecar", sidecar, "-dir", outDir,
	}
	if err := run(t.Context(), argv); err != nil {
		t.Fatalf("run publish-layout: %v", err)
	}

	want := `{"fs":{"b":3,"f":"fs/f","q":{"a":2,"z":1}},"st":{"inner":{"a":2,"z":1},"b":3}}`
	if got := compactData(t, filepath.Join(outDir, "pipeline_outs.json")); got != want {
		t.Errorf("pipeline_outs.json = %s, want %s", got, want)
	}
}

// TestRunEntryArgsSortsOverrideKeys checks an entry-input override takes
// sorted keys whatever order the params file lists them in, as mrp encodes
// the invocation literal it stands in for, while a baked default keeps its
// bundle's order.
func TestRunEntryArgsSortsOverrideKeys(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base")
	writeRawBundle(t, base, `{"d":{"b":1,"a":2},"o":null}`)
	values := writeTestFile(t, filepath.Join(dir, "values.json"), `{"o":{"z":1,"a":{"y":1,"x":2}}}`)
	out := filepath.Join(dir, "out")

	if err := run(t.Context(), []string{"entryargs", "-base", base, "-values", values, "-o", out}); err != nil {
		t.Fatalf("run entryargs: %v", err)
	}

	want := `{"d":{"b":1,"a":2},"o":{"a":{"x":2,"y":1},"z":1}}`
	if got := compactData(t, filepath.Join(out, "data.json")); got != want {
		t.Errorf("entry args = %s, want %s", got, want)
	}
}
