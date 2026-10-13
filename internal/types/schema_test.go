package types_test

import (
	"errors"
	"testing"

	"github.com/eunmann/mro2nf/internal/types"
	"github.com/google/go-cmp/cmp"
	"github.com/martian-lang/martian/martian/syntax"
)

// TestSchema pins the JSON Schema of each Martian type class: the scalars,
// a file of each kind, arrays of any dimension, untyped and typed maps, and
// structs, nested ones and those holding files.
func TestSchema(t *testing.T) {
	tbl := newTable(t)

	str := func(format string) *types.Schema { return &types.Schema{Type: "string", Format: format} }
	integer := &types.Schema{Type: "integer"}
	cfg := &types.Schema{Type: "object", Properties: map[string]*types.Schema{"ref": str("file-path"), "n": integer}}

	cases := []struct {
		tname string
		want  *types.Schema
	}{
		{"int", integer},
		{"float", &types.Schema{Type: "number"}},
		{"bool", &types.Schema{Type: "boolean"}},
		{"string", str("")},
		{"file", str("file-path")},
		{"txt", str("file-path")},
		{"path", str("path")},
		{"map", &types.Schema{Type: "object"}},
		{"int[]", &types.Schema{Type: "array", Items: integer}},
		{"txt[][]", &types.Schema{Type: "array", Items: &types.Schema{Type: "array", Items: str("file-path")}}},
		{"map<float>", &types.Schema{Type: "object", AdditionalProperties: &types.Schema{Type: "number"}}},
		{"map<bam[]>", &types.Schema{Type: "object", AdditionalProperties: &types.Schema{Type: "array", Items: str("file-path")}}},
		{"Cfg", cfg},
		{"Outer", &types.Schema{Type: "object", Properties: map[string]*types.Schema{"inner": cfg, "log": str("file-path")}}},
		{"Notes", &types.Schema{Type: "object", Properties: map[string]*types.Schema{"note": str(""), "extra": {Type: "object"}}}},
		{"Cfg[]", &types.Schema{Type: "array", Items: cfg}},
	}

	for _, tc := range cases {
		var id syntax.TypeId
		if err := id.UnmarshalText([]byte(tc.tname)); err != nil {
			t.Fatalf("type %s: %v", tc.tname, err)
		}

		got, err := tbl.Schema(id)
		if err != nil {
			t.Errorf("%s: %v", tc.tname, err)

			continue
		}

		if diff := cmp.Diff(tc.want, got); diff != "" {
			t.Errorf("%s schema (-want +got):\n%s", tc.tname, diff)
		}
	}
}

// TestSplitSchema: Martian's invocation form of a split is an object whose
// required split member is an array or a map of the element schema.
func TestSplitSchema(t *testing.T) {
	elem := &types.Schema{Type: "string", Format: "file-path"}
	want := &types.Schema{
		Type: "object",
		Properties: map[string]*types.Schema{"split": {AnyOf: []*types.Schema{
			{Type: "array", Items: elem}, {Type: "object", AdditionalProperties: elem},
		}}},
		Required: []string{"split"},
	}

	if diff := cmp.Diff(want, types.Split(elem)); diff != "" {
		t.Errorf("split schema (-want +got):\n%s", diff)
	}
}

// TestSchemaUnknownType: a type the table lacks has no schema.
func TestSchemaUnknownType(t *testing.T) {
	if _, err := newTable(t).Schema(syntax.TypeId{Tname: "Missing"}); !errors.Is(err, types.ErrUnknownType) {
		t.Errorf("Schema(Missing) = %v, want ErrUnknownType", err)
	}
}
