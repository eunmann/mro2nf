package types_test

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/types"
	"github.com/google/go-cmp/cmp"
)

// upper rewrites a path to its uppercase form, so tests can assert exactly which
// leaves the walk reached.
func upper(s string) (string, error) { return strings.ToUpper(s), nil }

// collector records every file leaf the walk visits (returning it unchanged).
func collector() (types.Transform, *[]string) {
	var seen []string

	fn := func(s string) (string, error) {
		seen = append(seen, s)

		return s, nil
	}

	return fn, &seen
}

// TestApplySkipLeaf guards bug 7: a Transform returning ErrSkipLeaf resolves that
// one leaf to null and continues the walk, rather than aborting it. publish uses
// this for a declared output file the pipeline legitimately never produced —
// matching Martian, which Lstats each published file and writes null when absent
// (core/post_process.go), instead of failing the whole run.
func TestApplySkipLeaf(t *testing.T) {
	fn := func(s string) (string, error) {
		if s == "skipme" {
			return "", types.ErrSkipLeaf
		}

		return "X:" + s, nil
	}

	params := []ir.Param{fileParam("a", 0, 0), fileParam("b", 0, 0)}
	vals := map[string]any{"a": "skipme", "b": "keep"}

	got, err := newTable().Apply(params, vals, fn)
	if err != nil {
		t.Fatalf("ErrSkipLeaf must not abort the walk: %v", err)
	}

	want := map[string]any{"a": nil, "b": "X:keep"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Apply with ErrSkipLeaf mismatch (-want +got):\n%s", diff)
	}
}

func newTable() *types.Table {
	return types.NewTable(map[string]*ir.StructType{
		"Cfg": {Name: "Cfg", Fields: []ir.Param{
			{Name: "ref", BaseType: "file", IsFile: true},
			{Name: "n", BaseType: "int"},
		}},
		"Outer": {Name: "Outer", Fields: []ir.Param{
			{Name: "inner", BaseType: "Cfg"},
			{Name: "log", BaseType: "file", IsFile: true},
		}},
	})
}

func fileParam(name string, arrayDim, mapDim int) ir.Param {
	return ir.Param{Name: name, BaseType: "file", IsFile: true, ArrayDim: arrayDim, MapDim: mapDim}
}

type leafCase struct {
	name   string
	params []ir.Param
	vals   map[string]any
	want   []string // file leaves, sorted
}

// leafShapeCases is a long-by-design table of file-leaf shapes.
func leafShapeCases() []leafCase {
	return []leafCase{
		{
			name:   "single file",
			params: []ir.Param{fileParam("f", 0, 0)},
			vals:   map[string]any{"f": "a.txt"},
			want:   []string{"a.txt"},
		},
		{
			name:   "file array",
			params: []ir.Param{fileParam("fs", 1, 0)},
			vals:   map[string]any{"fs": []any{"a", "b"}},
			want:   []string{"a", "b"},
		},
		{
			name:   "map of file",
			params: []ir.Param{fileParam("m", 0, 1)},
			vals:   map[string]any{"m": map[string]any{"k1": "x", "k2": "y"}},
			want:   []string{"x", "y"},
		},
		{
			// Same flattened dims as the real map<file[]> but the runtime value is
			// an array-of-maps; the shape-driven walk must handle both.
			name:   "array of file map",
			params: []ir.Param{fileParam("a", 1, 1)},
			vals:   map[string]any{"a": []any{map[string]any{"k": "p"}, map[string]any{"k": "q"}}},
			want:   []string{"p", "q"},
		},
		{
			// The REAL lowering of map<file[]>: Martian folds the inner array dim
			// into MapDim, so the IR is {ArrayDim:0, MapDim:2}. The walk must reach
			// both leaves; the old nested-map-level model dropped them silently.
			name:   "map of file array (real lowering, MapDim=2)",
			params: []ir.Param{fileParam("m", 0, 2)},
			vals:   map[string]any{"m": map[string]any{"k": []any{"p", "q"}}},
			want:   []string{"p", "q"},
		},
		{
			name:   "map of file matrix (map<file[][]>, MapDim=3)",
			params: []ir.Param{fileParam("m", 0, 3)},
			vals:   map[string]any{"m": map[string]any{"k": []any{[]any{"a", "b"}, []any{"c"}}}},
			want:   []string{"a", "b", "c"},
		},
		{
			name:   "non-file primitive untouched",
			params: []ir.Param{{Name: "n", BaseType: "int"}},
			vals:   map[string]any{"n": float64(5)},
			want:   nil,
		},
		{
			name:   "struct with file field",
			params: []ir.Param{{Name: "c", BaseType: "Cfg"}},
			vals:   map[string]any{"c": map[string]any{"ref": "r.bam", "n": float64(1)}},
			want:   []string{"r.bam"},
		},
		{
			// Martian marks a struct that contains files as a directory kind
			// (IsFile true); the walk must still descend it, not treat it as a
			// file leaf.
			name:   "struct-with-file param marked IsFile",
			params: []ir.Param{{Name: "c", BaseType: "Cfg", IsFile: true}},
			vals:   map[string]any{"c": map[string]any{"ref": "r.bam", "n": float64(1)}},
			want:   []string{"r.bam"},
		},
		{
			name:   "array of struct",
			params: []ir.Param{{Name: "cs", BaseType: "Cfg", ArrayDim: 1}},
			vals: map[string]any{"cs": []any{
				map[string]any{"ref": "a", "n": float64(1)},
				map[string]any{"ref": "b", "n": float64(2)},
			}},
			want: []string{"a", "b"},
		},
		{
			name:   "nested struct",
			params: []ir.Param{{Name: "o", BaseType: "Outer"}},
			vals: map[string]any{"o": map[string]any{
				"inner": map[string]any{"ref": "deep.txt", "n": float64(1)},
				"log":   "top.log",
			}},
			want: []string{"deep.txt", "top.log"},
		},
		{
			name:   "null file tolerated",
			params: []ir.Param{fileParam("f", 0, 0)},
			vals:   map[string]any{"f": nil},
			want:   nil,
		},
	}
}

func TestApplyLeafShapes(t *testing.T) {
	tbl := newTable()

	for _, tc := range leafShapeCases() {
		t.Run(tc.name, func(t *testing.T) {
			fn, seen := collector()

			if _, err := tbl.Apply(tc.params, tc.vals, fn); err != nil {
				t.Fatalf("Apply: %v", err)
			}

			got := append([]string(nil), *seen...)
			sort.Strings(got)
			sort.Strings(tc.want)

			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("leaves mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestApplyRewrites verifies the transform result is threaded back into the
// returned structure, at every nesting depth, while non-file data is preserved.
func TestApplyRewrites(t *testing.T) {
	tbl := newTable()
	vals := map[string]any{
		"o": map[string]any{
			"inner": map[string]any{"ref": "deep.txt", "n": float64(7)},
			"log":   "top.log",
		},
		"keep": "untouched",
	}

	got, err := tbl.Apply([]ir.Param{{Name: "o", BaseType: "Outer"}}, vals, upper)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	want := map[string]any{
		"o": map[string]any{
			"inner": map[string]any{"ref": "DEEP.TXT", "n": float64(7)},
			"log":   "TOP.LOG",
		},
		"keep": "untouched",
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("rewrite mismatch (-want +got):\n%s", diff)
	}
}

// TestApplyMapDeterministic checks that a map's file leaves are visited in a
// stable (sorted-key) order so bundle layout is reproducible across runs.
func TestApplyMapDeterministic(t *testing.T) {
	tbl := newTable()
	vals := map[string]any{"m": map[string]any{"z": "zf", "a": "af", "m": "mf"}}
	params := []ir.Param{fileParam("m", 0, 1)}

	for range 5 {
		fn, seen := collector()
		if _, err := tbl.Apply(params, vals, fn); err != nil {
			t.Fatalf("Apply: %v", err)
		}

		if diff := cmp.Diff([]string{"af", "mf", "zf"}, *seen); diff != "" {
			t.Errorf("visit order not sorted/stable (-want +got):\n%s", diff)
		}
	}
}

// TestApplyUnknownKeysPreserved checks that values without a declared param
// survive the walk unchanged.
func TestApplyUnknownKeysPreserved(t *testing.T) {
	tbl := newTable()
	vals := map[string]any{"declared": "a.txt", "extra": float64(9)}

	got, err := tbl.Apply([]ir.Param{fileParam("declared", 0, 0)}, vals, upper)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if got["extra"] != float64(9) || got["declared"] != "A.TXT" {
		t.Errorf("got %v, want declared=A.TXT extra=9", got)
	}
}

// TestFormatFloats checks float leaves at every depth take Martian's 'g', -1
// encoding while int leaves and the caller's input are left untouched.
func TestFormatFloats(t *testing.T) {
	tbl := newTable()
	params := []ir.Param{
		{Name: "f", BaseType: "float"},
		{Name: "i", BaseType: "int"},
		{Name: "m", BaseType: "float", MapDim: 2},
	}
	in := []any{json.Number("2.0")}
	vals := map[string]any{
		"f": json.Number("1E6"),
		"i": json.Number("2.0"),
		"m": map[string]any{"k": in},
	}

	got := tbl.FormatFloats(params, vals)

	want := map[string]any{
		"f": json.Number("1e+06"),
		"i": json.Number("2.0"),
		"m": map[string]any{"k": []any{json.Number("2")}},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("FormatFloats mismatch (-want +got):\n%s", diff)
	}

	if in[0] != json.Number("2.0") {
		t.Errorf("FormatFloats mutated its input: %v", in)
	}
}

// TestFormatFloatsUntypedMap checks a float-literal number anywhere inside an
// untyped map takes Martian's 'g', -1 encoding, as a FloatExp in an .mro map
// literal does under mrp, while int-literal numbers keep their text.
func TestFormatFloatsUntypedMap(t *testing.T) {
	tbl := newTable()
	params := []ir.Param{
		{Name: "um", BaseType: "map"},
		{Name: "ums", BaseType: "map", ArrayDim: 1},
	}
	vals := map[string]any{
		"um": map[string]any{
			"x":    json.Number("2.0"),
			"n":    json.Number("7"),
			"big":  json.Number("12345678901234567890"),
			"deep": map[string]any{"a": []any{json.Number("1E6"), "s"}},
		},
		"ums": []any{map[string]any{"y": json.Number("0.50")}},
	}

	got := tbl.FormatFloats(params, vals)

	want := map[string]any{
		"um": map[string]any{
			"x":    json.Number("2"),
			"n":    json.Number("7"),
			"big":  json.Number("12345678901234567890"),
			"deep": map[string]any{"a": []any{json.Number("1e+06"), "s"}},
		},
		"ums": []any{map[string]any{"y": json.Number("0.5")}},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("FormatFloats mismatch (-want +got):\n%s", diff)
	}
}
