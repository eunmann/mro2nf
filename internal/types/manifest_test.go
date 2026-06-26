package types_test

import (
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/eunmann/martian-nextflow/internal/ir"
	"github.com/eunmann/martian-nextflow/internal/types"
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
