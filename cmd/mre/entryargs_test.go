package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/eunmann/martian-nextflow/internal/ir"
	"github.com/eunmann/martian-nextflow/internal/types"
)

// TestOverlayValues checks the value-overlay merge: a supplied value replaces the
// baked default, a null (or absent) value keeps the default, and whole-number
// floats survive as json.Number so type coercion downstream sees the original.
func TestOverlayValues(t *testing.T) {
	payload := map[string]any{
		"a": json.Number("1"),
		"b": json.Number("2"),
		"c": json.Number("3"),
	}

	overlayValues(payload, map[string]any{
		"a": json.Number("5"),
		"b": nil,
		"d": json.Number("21.0"),
	})

	if got := fmt.Sprint(payload["a"]); got != "5" {
		t.Errorf("a = %s, want 5 (overridden)", got)
	}
	if got := fmt.Sprint(payload["b"]); got != "2" {
		t.Errorf("b = %s, want 2 (null override keeps the baked default)", got)
	}
	if got := fmt.Sprint(payload["c"]); got != "3" {
		t.Errorf("c = %s, want 3 (untouched)", got)
	}
	if _, ok := payload["d"].(json.Number); !ok {
		t.Errorf("d = %v (%T), want a json.Number (whole-number float not collapsed)", payload["d"], payload["d"])
	}
}

// entryTable builds a type table + params covering a scalar file, a file array,
// and a struct with a file field — the shapes file-typed entry inputs take.
func entryTable() (*types.Table, []ir.Param) {
	tbl := types.NewTable(map[string]*ir.StructType{
		"Cfg": {Name: "Cfg", Fields: []ir.Param{
			{Name: "ref", BaseType: "file", IsFile: true},
			{Name: "n", BaseType: "int"},
		}},
	})
	params := []ir.Param{
		{Name: "reads", BaseType: "file", IsFile: true},
		{Name: "fastqs", BaseType: "file", ArrayDim: 1, IsFile: true},
		{Name: "cfg", BaseType: "Cfg", IsFile: true},
		{Name: "scale", BaseType: "float"},
	}

	return tbl, params
}

// TestReconstructFiles checks the unified file reconstruction: for each supplied
// file-bearing input, the override's file leaves are replaced with the staged
// paths in canonical order (scalar, array in order, struct field), the key is
// removed from over (so the raw value is not re-copied), and an unset input keeps
// its baked default.
func TestReconstructFiles(t *testing.T) {
	tbl, params := entryTable()

	payload := map[string]any{
		"reads":  "baked/reads.txt",
		"fastqs": []any{"baked/x"},
		"cfg":    map[string]any{"ref": "baked/ref", "n": json.Number("9")},
		"scale":  json.Number("2"),
	}
	over := map[string]any{
		"reads":  "s3://b/reads.fastq",
		"fastqs": []any{"s3://b/a.fastq", "s3://b/b.fastq"},
		"cfg":    nil, // unset → keep baked
		"scale":  json.Number("5"),
	}
	flatMap := map[string][]string{
		"reads":  {"/work/reads.fastq"},
		"fastqs": {"/work/a.fastq", "/work/b.fastq"},
		"cfg":    {"/work/unused"},
	}

	if err := reconstructFiles(payload, over, flatMap, params, tbl); err != nil {
		t.Fatalf("reconstructFiles: %v", err)
	}

	if got := payload["reads"]; got != "/work/reads.fastq" {
		t.Errorf("reads = %v, want the staged path", got)
	}
	if got := payload["fastqs"]; !reflect.DeepEqual(got, []any{"/work/a.fastq", "/work/b.fastq"}) {
		t.Errorf("fastqs = %v, want the two staged paths in order", got)
	}
	cfg, _ := payload["cfg"].(map[string]any)
	if got := cfg["ref"]; got != "baked/ref" {
		t.Errorf("cfg.ref = %v, want the baked default (cfg was unset)", got)
	}

	// reconstructed keys are removed from over; non-file values remain.
	for _, k := range []string{"reads", "fastqs"} {
		if _, ok := over[k]; ok {
			t.Errorf("over[%q] should have been consumed", k)
		}
	}
	if _, ok := over["scale"]; !ok {
		t.Error("over[scale] (a non-file value) should remain for overlayValues")
	}
}

// TestReconstructFilesStruct checks a struct file field is replaced while sibling
// scalar fields are preserved.
func TestReconstructFilesStruct(t *testing.T) {
	tbl, params := entryTable()

	payload := map[string]any{}
	over := map[string]any{"cfg": map[string]any{"ref": "s3://b/ref.csv", "n": json.Number("3")}}
	flatMap := map[string][]string{"cfg": {"/work/ref.csv"}}

	if err := reconstructFiles(payload, over, flatMap, params, tbl); err != nil {
		t.Fatalf("reconstructFiles: %v", err)
	}

	cfg, ok := payload["cfg"].(map[string]any)
	if !ok {
		t.Fatalf("cfg = %v, want a map", payload["cfg"])
	}
	if cfg["ref"] != "/work/ref.csv" {
		t.Errorf("cfg.ref = %v, want the staged path", cfg["ref"])
	}
	if fmt.Sprint(cfg["n"]) != "3" {
		t.Errorf("cfg.n = %v, want 3 (non-file field preserved)", cfg["n"])
	}
}

// TestReconstructFilesStagedCountMismatch checks the staged-count guards: fewer
// staged paths than file leaves fails (errStagedFew) and more staged paths than
// leaves fails (errStagedMany) — both signal the override value and the staged
// paths disagree on shape, which would otherwise mis-bind or drop files silently.
func TestReconstructFilesStagedCountMismatch(t *testing.T) {
	tbl, params := entryTable()

	// A 2-element file array but only one staged path: too few.
	few := reconstructFiles(
		map[string]any{}, map[string]any{"fastqs": []any{"s3://b/a", "s3://b/b"}},
		map[string][]string{"fastqs": {"/work/a"}}, params, tbl)
	if !errors.Is(few, errStagedFew) {
		t.Errorf("too-few staged: err = %v, want errStagedFew", few)
	}

	// A single scalar file leaf but two staged paths: too many.
	many := reconstructFiles(
		map[string]any{}, map[string]any{"reads": "s3://b/r"},
		map[string][]string{"reads": {"/work/r1", "/work/r2"}}, params, tbl)
	if !errors.Is(many, errStagedMany) {
		t.Errorf("too-many staged: err = %v, want errStagedMany", many)
	}
}

// TestParseFlatFlags checks the -fileflat parser, including multi-input, empty,
// and malformed cases.
func TestParseFlatFlags(t *testing.T) {
	got, err := parseFlatFlags("reads=/w/a;fastqs=/w/x,/w/y")
	if err != nil {
		t.Fatalf("parseFlatFlags: %v", err)
	}
	if !reflect.DeepEqual(got["reads"], []string{"/w/a"}) {
		t.Errorf("reads = %v, want [/w/a]", got["reads"])
	}
	if !reflect.DeepEqual(got["fastqs"], []string{"/w/x", "/w/y"}) {
		t.Errorf("fastqs = %v, want the two staged paths", got["fastqs"])
	}

	if empty, err := parseFlatFlags(""); err != nil || len(empty) != 0 {
		t.Errorf("parseFlatFlags(\"\") = %v, %v; want empty map, nil", empty, err)
	}

	if _, err := parseFlatFlags("bogus"); err == nil {
		t.Error("parseFlatFlags(\"bogus\") should error (no name=path)")
	}
}
