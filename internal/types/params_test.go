package types_test

import (
	"encoding/json"
	"testing"

	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/types"
	"github.com/google/go-cmp/cmp"
)

// jsonNum builds a json.Number literal (the decode-with-UseNumber shape the
// walk sees at runtime).
func jsonNum(t *testing.T, s string) json.Number {
	t.Helper()

	return json.Number(s)
}

// TestManifestParams pins the role -> parameter-set selection every bundle
// producer relies on; its only callers live in cmd/mre, so a regression here
// (e.g. RoleMainOut losing the ChunkOut concat) would surface only as missing
// file staging in a live run.
func TestManifestParams(t *testing.T) {
	man := types.Manifest{Callables: map[string]types.Callable{
		"S": {
			In:       []ir.Param{{Name: "in1"}},
			Out:      []ir.Param{{Name: "out1"}},
			ChunkIn:  []ir.Param{{Name: "cin1"}},
			ChunkOut: []ir.Param{{Name: "cout1"}},
		},
	}}

	names := func(ps []ir.Param) []string {
		out := make([]string, 0, len(ps))
		for _, p := range ps {
			out = append(out, p.Name)
		}

		return out
	}

	cases := []struct {
		callable, role string
		want           []string
	}{
		{"S", types.RoleIn, []string{"in1"}},
		{"S", types.RoleOut, []string{"out1"}},
		{"S", types.RoleChunkIn, []string{"cin1"}},
		{"S", types.RoleMainOut, []string{"out1", "cout1"}},
		{"S", "bogus-role", nil},
		{"MISSING", types.RoleIn, nil},
	}

	for _, tc := range cases {
		got := names(man.Params(tc.callable, tc.role))
		if tc.want == nil {
			if len(got) != 0 {
				t.Errorf("Params(%s,%s) = %v, want empty", tc.callable, tc.role, got)
			}

			continue
		}

		if diff := cmp.Diff(tc.want, got); diff != "" {
			t.Errorf("Params(%s,%s) mismatch (-want +got):\n%s", tc.callable, tc.role, diff)
		}
	}
}

// TestCoerceScalarsStructField checks a whole-number float inside a struct's
// int field coerces through the struct recursion (previously untested).
func TestCoerceScalarsStructField(t *testing.T) {
	tbl := types.NewTable(map[string]*ir.StructType{
		"Cfg": {Name: "Cfg", Fields: []ir.Param{{Name: "n", BaseType: "int"}, {Name: "s", BaseType: "string"}}},
	})

	got := tbl.CoerceScalars(
		[]ir.Param{{Name: "c", BaseType: "Cfg"}},
		map[string]any{"c": map[string]any{"n": jsonNum(t, "5.0"), "s": "x"}},
	)

	inner, ok := got["c"].(map[string]any)
	if !ok {
		t.Fatalf("c = %T, want map", got["c"])
	}

	if n, ok := inner["n"].(int64); !ok || n != 5 {
		t.Errorf("c.n = %v (%T), want int64(5)", inner["n"], inner["n"])
	}
}

// TestWalkShapeMismatchPassthrough checks values whose runtime shape disagrees
// with the declared type pass through untouched instead of erroring — runtime
// values may legitimately be null or oddly-shaped mid-pipeline.
func TestWalkShapeMismatchPassthrough(t *testing.T) {
	tbl := types.NewTable(nil)

	calls := 0
	fn := func(p string) (string, error) {
		calls++

		return p, nil
	}

	vals := map[string]any{
		"arrAsScalar": []any{"a"},               // array value, scalar-typed param
		"numAsFile":   jsonNum(t, "7"),          // number where a file string is declared
		"mapAsFile":   map[string]any{"k": "v"}, // object for a scalar file (unknown struct)
	}
	params := []ir.Param{
		{Name: "arrAsScalar", BaseType: "txt", IsFile: true},
		{Name: "numAsFile", BaseType: "txt", IsFile: true},
		{Name: "mapAsFile", BaseType: "txt", IsFile: true},
	}

	got, err := tbl.Apply(params, vals, fn)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if calls != 0 {
		t.Errorf("transform ran %d times on mismatched shapes, want 0", calls)
	}

	if diff := cmp.Diff(vals, got); diff != "" {
		t.Errorf("mismatched shapes were rewritten (-want +got):\n%s", diff)
	}
}
