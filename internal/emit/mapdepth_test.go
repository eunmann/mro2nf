package emit

import (
	"errors"
	"testing"

	"github.com/eunmann/mro2nf/internal/apperror"
	"github.com/eunmann/mro2nf/internal/ir"
)

// TestCheckSupportedRejectsArrayOfMapProjection verifies the emit-time guard:
// projecting a struct field through an array of typed maps (array<map<S>>.field)
// has no faithful lowering, so checkSupported must reject it (rather than emit
// silently-wrong wiring) with an ErrUnsupported.
func TestCheckSupportedRejectsArrayOfMapProjection(t *testing.T) {
	prog := &ir.Program{
		Structs: map[string]*ir.StructType{
			"P": {Name: "P", Fields: []ir.Param{{Name: "x", BaseType: "int"}}},
		},
		Stages: map[string]*ir.Stage{
			"MAPOUT": {Name: "MAPOUT", Out: []ir.Param{{Name: "m", BaseType: "P", MapDim: 1}}},
		},
		Pipelines: map[string]*ir.Pipeline{
			"T": {
				Name: "T",
				Out:  []ir.Param{{Name: "r", BaseType: "int", ArrayDim: 1, MapDim: 1}},
				Calls: []ir.Call{
					{Name: "ARRFORK", Callable: "MAPOUT", Mapped: true, MapMode: "array"},
				},
				// return r = ARRFORK.m.x  -> array<map<P>>.x projection
				Returns: []ir.Binding{{
					Param: "r",
					Value: ir.Value{Ref: &ir.Ref{Kind: "call", ID: "ARRFORK", Output: "m.x"}},
				}},
			},
		},
	}

	err := checkSupported(prog)
	if err == nil {
		t.Fatal("checkSupported accepted array<map<S>>.field projection")
	}

	if !errors.Is(err, apperror.ErrUnsupported) {
		t.Errorf("error = %v, want ErrUnsupported", err)
	}
}

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
			"MAPOUT":    {Name: "MAPOUT", Out: []ir.Param{{Name: "m", BaseType: "P", MapDim: 1}}},
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
		{"array-of-map<S> field proj (unsupported)", &ir.Ref{Kind: "call", ID: "ARRFORK", Output: "m.x"}, -1},
		{"array-fork over map<S>, no field proj", &ir.Ref{Kind: "call", ID: "ARRFORK", Output: "m"}, 0},
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
