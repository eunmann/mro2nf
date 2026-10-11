package types_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/types"
	"github.com/google/go-cmp/cmp"
)

func TestManifestRoundTrip(t *testing.T) {
	prog := &ir.Program{
		Structs: map[string]*ir.StructType{
			"Cfg": {Name: "Cfg", Fields: []ir.Param{{Name: "ref", BaseType: "file", IsFile: true}}},
		},
		Stages: map[string]*ir.Stage{
			"S": {
				Name:     "S",
				In:       []ir.Param{{Name: "x", BaseType: "int"}},
				Out:      []ir.Param{{Name: "f", BaseType: "file", IsFile: true}},
				ChunkOut: []ir.Param{{Name: "c", BaseType: "file", IsFile: true}},
			},
		},
		Pipelines: map[string]*ir.Pipeline{
			"P": {Name: "P", Out: []ir.Param{{Name: "o", BaseType: "Cfg"}}},
		},
	}

	path := filepath.Join(t.TempDir(), "types.json")
	if err := types.BuildManifest(prog).Write(path); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := types.LoadManifest(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if diff := cmp.Diff(prog.Structs, got.Structs); diff != "" {
		t.Errorf("structs round-trip mismatch (-want +got):\n%s", diff)
	}

	if diff := cmp.Diff(prog.Stages["S"].ChunkOut, got.Callables["S"].ChunkOut); diff != "" {
		t.Errorf("stage chunkOut mismatch (-want +got):\n%s", diff)
	}

	if got.Callables["P"].Out[0].BaseType != "Cfg" {
		t.Errorf("pipeline P out[0] BaseType = %q, want Cfg", got.Callables["P"].Out[0].BaseType)
	}

	// The loaded manifest yields a working file-leaf walk table.
	seen := map[string]bool{}

	record := func(s string) (string, error) {
		seen[s] = true

		return s, nil
	}

	if _, err := got.Table().Apply(got.Callables["S"].Out, map[string]any{"f": "a.txt"}, record); err != nil {
		t.Fatalf("apply: %v", err)
	}

	if !seen["a.txt"] {
		t.Error("table from loaded manifest did not visit file leaf")
	}
}

// TestManifestSubset checks a task's manifest holds only its callables and
// the structs they reach, through any parameter set and nested struct, so a
// change to another callable's types leaves the task's cache key alone (#286).
func TestManifestSubset(t *testing.T) {
	inner := &ir.StructType{Name: "Inner", Fields: []ir.Param{{Name: "f", BaseType: "file", IsFile: true}}}
	outer := &ir.StructType{Name: "Outer", Fields: []ir.Param{{Name: "in", BaseType: "Inner", MapDim: 1}}}
	chunk := &ir.StructType{Name: "Chunk", Fields: []ir.Param{{Name: "n", BaseType: "int"}}}
	other := &ir.StructType{Name: "Other", Fields: []ir.Param{{Name: "n", BaseType: "int"}}}
	a := types.Callable{
		In:       []ir.Param{{Name: "x", BaseType: "Outer", ArrayDim: 1}},
		ChunkOut: []ir.Param{{Name: "c", BaseType: "Chunk"}},
	}
	b := types.Callable{In: []ir.Param{{Name: "y", BaseType: "Other"}}}
	m := types.Manifest{
		Structs:   map[string]*ir.StructType{"Inner": inner, "Outer": outer, "Chunk": chunk, "Other": other},
		Callables: map[string]types.Callable{"A": a, "B": b},
	}

	want := types.Manifest{
		Structs:   map[string]*ir.StructType{"Inner": inner, "Outer": outer, "Chunk": chunk},
		Callables: map[string]types.Callable{"A": a},
	}

	if diff := cmp.Diff(want, m.Subset("A", "MISSING")); diff != "" {
		t.Errorf("Subset(A) mismatch (-want +got):\n%s", diff)
	}

	enc, err := m.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	path := filepath.Join(t.TempDir(), "types.json")
	if err := os.WriteFile(path, []byte(enc.Subset("A", "MISSING")), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := types.LoadManifest(path)
	if err != nil {
		t.Fatalf("load the encoded subset: %v", err)
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("encoded Subset(A) mismatch (-want +got):\n%s", diff)
	}
}
