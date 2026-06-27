package types_test

import (
	"encoding/json"
	"math"
	"sort"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/types"
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
			name:   "map of file array",
			params: []ir.Param{fileParam("m", 1, 1)},
			vals:   map[string]any{"m": map[string]any{"k": []any{"p", "q"}}},
			want:   []string{"p", "q"},
		},
		{
			// Same flattened dims as "map of file array" but the runtime value is
			// an array-of-maps; the shape-driven walk must handle both.
			name:   "array of file map",
			params: []ir.Param{fileParam("a", 1, 1)},
			vals:   map[string]any{"a": []any{map[string]any{"k": "p"}, map[string]any{"k": "q"}}},
			want:   []string{"p", "q"},
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

// TestCoerceScalars checks whole-number floats bound to int params become
// integers, while float params and non-whole values are left intact.
func TestCoerceScalars(t *testing.T) {
	tbl := newTable()
	params := []ir.Param{
		{Name: "i", BaseType: "int"},
		{Name: "f", BaseType: "float"},
		{Name: "is", BaseType: "int", ArrayDim: 1},
	}
	vals := map[string]any{
		"i":  json.Number("5.0"),
		"f":  json.Number("42.0"),
		"is": []any{json.Number("1.0"), json.Number("2")},
	}

	got := tbl.CoerceScalars(params, vals)

	if got["i"] != int64(5) {
		t.Errorf("int leaf 5.0 -> %v (%T), want int64 5", got["i"], got["i"])
	}

	// An out-of-int64-range whole float is left as-is, not overflowed.
	big := tbl.CoerceScalars([]ir.Param{{Name: "b", BaseType: "int"}},
		map[string]any{"b": json.Number("1e20")})
	if big["b"] != json.Number("1e20") {
		t.Errorf("out-of-range int leaf -> %v, want unchanged 1e20", big["b"])
	}

	// Boundary: 2^63 == float64(math.MaxInt64) (which rounds up); int64(2^63)
	// wraps to MinInt64, so it must be left as the original number, not coerced.
	boundary := tbl.CoerceScalars([]ir.Param{{Name: "b", BaseType: "int"}},
		map[string]any{"b": json.Number("9223372036854775808")})
	if boundary["b"] != json.Number("9223372036854775808") {
		t.Errorf("2^63 int leaf -> %v (%T), want unchanged (would overflow to MinInt64)",
			boundary["b"], boundary["b"])
	}

	// MinInt64 (-2^63) is exactly representable and must coerce, not be dropped.
	minv := tbl.CoerceScalars([]ir.Param{{Name: "b", BaseType: "int"}},
		map[string]any{"b": json.Number("-9223372036854775808.0")})
	if minv["b"] != int64(math.MinInt64) {
		t.Errorf("-2^63 int leaf -> %v (%T), want int64 MinInt64", minv["b"], minv["b"])
	}

	if got["f"] != json.Number("42.0") {
		t.Errorf("float leaf 42.0 -> %v, want unchanged 42.0", got["f"])
	}

	arr, ok := got["is"].([]any)
	if !ok || arr[0] != int64(1) || arr[1] != int64(2) {
		t.Errorf("int[] -> %v, want [1 2] as int64", got["is"])
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
