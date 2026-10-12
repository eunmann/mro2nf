package frontend

import (
	"testing"

	"github.com/martian-lang/martian/martian/syntax"
)

// TestRenderType covers the typed-map and combined wrappers beyond the plain
// array case the external test pins.
func TestRenderType(t *testing.T) {
	cases := []struct {
		id   syntax.TypeId
		want string
	}{
		{syntax.TypeId{Tname: "int"}, "int"},
		{syntax.TypeId{Tname: "int", MapDim: 1}, "map<int>"},
		{syntax.TypeId{Tname: "int", MapDim: 2}, "map<int[]>"},
		{syntax.TypeId{Tname: "int", MapDim: 1, ArrayDim: 1}, "map<int>[]"},
		{syntax.TypeId{Tname: "txt", ArrayDim: 2}, "txt[][]"},
	}

	for _, tc := range cases {
		if got := renderType(tc.id); got != tc.want {
			t.Errorf("renderType(%+v) = %q, want %q", tc.id, got, tc.want)
		}
	}
}
