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
	}

	for _, tc := range cases {
		params, err := man.Params(tc.callable, tc.role)
		if err != nil {
			t.Errorf("Params(%s,%s): unexpected error %v", tc.callable, tc.role, err)

			continue
		}

		if diff := cmp.Diff(tc.want, names(params)); diff != "" {
			t.Errorf("Params(%s,%s) mismatch (-want +got):\n%s", tc.callable, tc.role, diff)
		}
	}
}

// TestManifestParamsUnknown checks a configured manifest fails loudly on an
// unknown callable or role instead of silently yielding no parameters (which
// would skip path rewrites and surface only as dangling files downstream).
func TestManifestParamsUnknown(t *testing.T) {
	man := types.Manifest{Callables: map[string]types.Callable{
		"S": {In: []ir.Param{{Name: "in1"}}},
	}}

	if _, err := man.Params("MISSING", types.RoleIn); err == nil {
		t.Error("Params(MISSING, in): want error, got nil")
	}

	if _, err := man.Params("S", "bogus-role"); err == nil {
		t.Error("Params(S, bogus-role): want error, got nil")
	}
}

// TestManifestParamsUnconfigured checks the zero manifest (the no `-types`
// path: nothing to rewrite) yields nil params without error for any callable.
func TestManifestParamsUnconfigured(t *testing.T) {
	var man types.Manifest

	params, err := man.Params("ANY", types.RoleIn)
	if err != nil {
		t.Fatalf("Params on unconfigured manifest: %v", err)
	}

	if params != nil {
		t.Errorf("Params on unconfigured manifest = %v, want nil", params)
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

// TestCoerceScalarsDoesNotMutateInput proves the copy-on-return contract holds
// for NESTED values too: coercing a whole-number float inside a caller-owned
// slice, typed map, or struct object must rewrite only the returned copy,
// never the input (callers reuse the decoded payload after coercion).
func TestCoerceScalarsDoesNotMutateInput(t *testing.T) {
	tbl := types.NewTable(map[string]*ir.StructType{
		"Cfg": {Name: "Cfg", Fields: []ir.Param{{Name: "n", BaseType: "int"}}},
	})

	arr := []any{json.Number("5.0")}
	cfg := map[string]any{"n": json.Number("7.0")}
	m := map[string]any{"k": json.Number("3.0")}
	vals := map[string]any{"arr": arr, "cfg": cfg, "m": m}
	params := []ir.Param{
		{Name: "arr", BaseType: "int", ArrayDim: 1},
		{Name: "cfg", BaseType: "Cfg"},
		{Name: "m", BaseType: "int", MapDim: 1},
	}

	got := tbl.CoerceScalars(params, vals)

	// The returned copy is coerced.
	want := map[string]any{
		"arr": []any{int64(5)},
		"cfg": map[string]any{"n": int64(7)},
		"m":   map[string]any{"k": int64(3)},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("coerced copy mismatch (-want +got):\n%s", diff)
	}

	// The caller-owned nested containers are untouched.
	if _, ok := arr[0].(json.Number); !ok {
		t.Errorf("input slice mutated: arr[0] = %v (%T), want json.Number", arr[0], arr[0])
	}
	if _, ok := cfg["n"].(json.Number); !ok {
		t.Errorf("input struct object mutated: cfg.n = %v (%T), want json.Number", cfg["n"], cfg["n"])
	}
	if _, ok := m["k"].(json.Number); !ok {
		t.Errorf("input map mutated: m.k = %v (%T), want json.Number", m["k"], m["k"])
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
