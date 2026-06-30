package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/google/go-cmp/cmp"
)

// TestPublishOutsTreeLayout pins the published outs/ tree to Martian's
// post_process layout: a scalar file is named by GetOutFilename at the root; an
// array becomes a subdir (bare param name) with zero-padded index filenames; a
// typed map a subdir with key filenames; a struct a subdir with each field named
// by its own GetOutFilename. Leaf JSON values are the path within the outs dir.
func TestPublishOutsTreeLayout(t *testing.T) {
	dir := t.TempDir()
	srcDir := t.TempDir()
	write := func(name string) string {
		p := filepath.Join(srcDir, name)
		if err := os.WriteFile(p, []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}

		return p
	}

	params := []ir.Param{
		{Name: "alignments", BaseType: "bam", IsFile: true},          // scalar user file -> name.bam
		{Name: "shards", BaseType: "csv", IsFile: true, ArrayDim: 1}, // array -> shards/<idx>.csv
		{Name: "reports", BaseType: "txt", IsFile: true, MapDim: 1},  // map -> reports/<key>.txt
		{Name: "cfg", BaseType: "MyStruct", IsFile: true},            // struct -> cfg/<field>
		{Name: "count", BaseType: "int"},                             // non-file scalar passes through
	}
	structs := map[string]*ir.StructType{
		"MyStruct": {Name: "MyStruct", Fields: []ir.Param{{Name: "data", BaseType: "file", IsFile: true}}},
	}
	outs := map[string]any{
		"alignments": write("aln.bam"),
		"shards":     []any{write("s0.csv"), write("s1.csv")},
		"reports":    map[string]any{"sampleA": write("ra.txt"), "sampleB": write("rb.txt")},
		"cfg":        map[string]any{"data": write("data.bin")},
		"count":      float64(7),
	}

	got, err := publishOuts(dir, params, structs, outs)
	if err != nil {
		t.Fatalf("publishOuts: %v", err)
	}

	want := map[string]any{
		"alignments": "alignments.bam",
		"shards":     []any{"shards/0.csv", "shards/1.csv"},
		"reports":    map[string]any{"sampleA": "reports/sampleA.txt", "sampleB": "reports/sampleB.txt"},
		"cfg":        map[string]any{"data": "cfg/data"},
		"count":      float64(7),
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("published outs mismatch (-want +got):\n%s", diff)
	}

	for _, rel := range []string{
		"alignments.bam", "shards/0.csv", "shards/1.csv",
		"reports/sampleA.txt", "reports/sampleB.txt", "cfg/data",
	} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Errorf("missing published file %s: %v", rel, err)
		}
	}
}

// TestPublishOutsAbsentFileNull guards the null cases: an empty-string leaf and a
// declared-but-never-written file both resolve to null, matching Martian.
func TestPublishOutsAbsentFileNull(t *testing.T) {
	dir := t.TempDir()
	params := []ir.Param{
		{Name: "missing", BaseType: "txt", IsFile: true},
		{Name: "empty", BaseType: "txt", IsFile: true},
	}
	outs := map[string]any{
		"missing": filepath.Join(dir, "never-written.txt"),
		"empty":   "",
	}

	got, err := publishOuts(dir, params, nil, outs)
	if err != nil {
		t.Fatalf("publishOuts: %v", err)
	}

	if got["missing"] != nil || got["empty"] != nil {
		t.Errorf("absent/empty leaves = %v/%v, want nil/nil", got["missing"], got["empty"])
	}
}

// TestPublishOutsArrayInStruct covers a struct field that is itself a file array
// (the struct_file_array fixture shape): the leaves nest under <param>/<field>/.
func TestPublishOutsArrayInStruct(t *testing.T) {
	dir := t.TempDir()
	srcDir := t.TempDir()
	w := func(n string) string {
		p := filepath.Join(srcDir, n)
		if err := os.WriteFile(p, []byte(n), 0o644); err != nil {
			t.Fatal(err)
		}

		return p
	}

	params := []ir.Param{{Name: "r", BaseType: "Report", IsFile: true}}
	structs := map[string]*ir.StructType{"Report": {Name: "Report", Fields: []ir.Param{
		{Name: "files", BaseType: "txt", IsFile: true, ArrayDim: 1},
		{Name: "n", BaseType: "int"},
	}}}
	outs := map[string]any{"r": map[string]any{"files": []any{w("a.txt"), w("b.txt")}, "n": float64(2)}}

	got, err := publishOuts(dir, params, structs, outs)
	if err != nil {
		t.Fatalf("publishOuts: %v", err)
	}

	want := map[string]any{"r": map[string]any{
		"files": []any{"r/files/0.txt", "r/files/1.txt"},
		"n":     float64(2),
	}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("array-in-struct publish mismatch (-want +got):\n%s", diff)
	}
}
