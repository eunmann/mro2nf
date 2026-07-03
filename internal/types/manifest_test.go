package types_test

import (
	"path/filepath"
	"sort"
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

// TestTableDescendsIntoCallableOutputBundle guards the bug where a param typed as
// a callable's whole output bundle (e.g. `matrix_computer_outs _SLFE_MATRIX_COMPUTER`,
// forwarding a sub-pipeline's outputs) left every file it carried unmarked. The
// callable name is not a declared struct, so the walk table must register each
// callable's outputs as a struct keyed by the callable name — otherwise the walk
// stops at the bundle and the nested files stay task-local paths that vanish on
// the next isolated worker. The bundle also nests a real struct to prove the walk
// keeps descending through both kinds.
func TestTableDescendsIntoCallableOutputBundle(t *testing.T) {
	prog := &ir.Program{
		Structs: map[string]*ir.StructType{
			// A file-typed struct nested inside the pipeline's outputs.
			"RnaChunk": {Name: "RnaChunk", Fields: []ir.Param{
				{Name: "r1", BaseType: "fastq", IsFile: true},
			}},
		},
		Pipelines: map[string]*ir.Pipeline{
			// A sub-pipeline whose outputs are a file, a file array, and an
			// array of a file-bearing struct — the shape of a real counter bundle.
			"SUB": {Name: "SUB", Out: []ir.Param{
				{Name: "matrix_h5", BaseType: "h5", IsFile: true},
				{Name: "shards", BaseType: "bincode", IsFile: true, ArrayDim: 1},
				{Name: "read_chunks", BaseType: "RnaChunk", ArrayDim: 1},
			}},
		},
		Stages: map[string]*ir.Stage{
			// A consumer that receives the whole sub-pipeline output as one input,
			// typed by the pipeline name (IsFile true: Martian marks a file-bearing
			// bundle as a directory kind).
			"CONSUMER": {Name: "CONSUMER", In: []ir.Param{
				{Name: "sub_outs", BaseType: "SUB", IsFile: true},
			}},
		},
	}

	man := types.BuildManifest(prog)

	fn, seen := collector()

	payload := map[string]any{
		"sub_outs": map[string]any{
			"matrix_h5":   "/scratch/matrix.h5",
			"shards":      []any{"/scratch/s0.bincode", "/scratch/s1.bincode"},
			"read_chunks": []any{map[string]any{"r1": "/scratch/r1.fastq"}},
		},
	}

	if _, err := man.Table().Apply(man.Callables["CONSUMER"].In, payload, fn); err != nil {
		t.Fatalf("apply: %v", err)
	}

	sort.Strings(*seen)

	want := []string{
		"/scratch/matrix.h5",
		"/scratch/r1.fastq",
		"/scratch/s0.bincode",
		"/scratch/s1.bincode",
	}
	if diff := cmp.Diff(want, *seen); diff != "" {
		t.Errorf("walk did not reach every file leaf inside the callable output bundle (-want +got):\n%s", diff)
	}
}
