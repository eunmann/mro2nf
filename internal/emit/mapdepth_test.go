package emit

import (
	"testing"

	"github.com/eunmann/martian-nextflow/internal/ir"
)

// TestMapProjectDepth covers the typed-map field-projection depth resolver,
// including the edge cases a code review surfaced: an unresolvable ref path must
// return 0 (not panic), and an array fork over a map<S> output must not be
// treated as a map projection.
func TestMapProjectDepth(t *testing.T) {
	prog := &ir.Program{
		Structs: map[string]*ir.StructType{
			"P": {Name: "P", Fields: []ir.Param{{Name: "x", BaseType: "int"}}},
		},
		Stages: map[string]*ir.Stage{
			"MAPOUT": {Name: "MAPOUT", Out: []ir.Param{{Name: "m", BaseType: "P", MapDim: 1}}},
			"STRUCTOUT": {Name: "STRUCTOUT", Out: []ir.Param{{Name: "p", BaseType: "P"}}},
		},
	}
	p := &ir.Pipeline{
		Name: "T",
		In:   []ir.Param{{Name: "mm", BaseType: "P", MapDim: 1}, {Name: "n", BaseType: "int"}},
		Calls: []ir.Call{
			{Name: "MO", Callable: "MAPOUT"},
			{Name: "MAPFORK", Callable: "STRUCTOUT", Mapped: true, MapMode: "map"},
			{Name: "ARRFORK", Callable: "MAPOUT", Mapped: true, MapMode: "array"},
		},
	}

	cases := []struct {
		name string
		ref  *ir.Ref
		want int
	}{
		{"declared map<S>.field", &ir.Ref{Kind: "call", ID: "MO", Output: "m.x"}, 1},
		{"self map<S>.field", &ir.Ref{Kind: "self", ID: "mm", Output: "x"}, 1},
		{"map-fork struct .field", &ir.Ref{Kind: "call", ID: "MAPFORK", Output: "p.x"}, 1},
		{"array-fork over map<S> (not a map proj)", &ir.Ref{Kind: "call", ID: "ARRFORK", Output: "m.x"}, 0},
		{"plain scalar self ref", &ir.Ref{Kind: "self", ID: "n"}, 0},
		{"unknown call (no panic)", &ir.Ref{Kind: "call", ID: "MISSING", Output: "a.b"}, 0},
		{"unknown self input (no panic)", &ir.Ref{Kind: "self", ID: "absent", Output: "a.b"}, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mapProjectDepth(prog, p, tc.ref); got != tc.want {
				t.Errorf("mapProjectDepth = %d, want %d", got, tc.want)
			}
		})
	}
}
