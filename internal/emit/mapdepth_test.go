package emit

import (
	"errors"
	"testing"

	"github.com/eunmann/mro2nf/internal/apperror"
	"github.com/eunmann/mro2nf/internal/ir"
)

// TestCheckSupported verifies the emit-time projection guard: array<map<S>>.field
// is supported (projected over each map within the array), while projecting a
// field through *nested* typed maps (map<map<S>>.field) has no faithful single-
// level lowering and must be rejected with ErrUnsupported rather than emitted as
// silently-wrong wiring.
func TestCheckSupported(t *testing.T) {
	mk := func(ref *ir.Ref) *ir.Program {
		return &ir.Program{
			Structs: map[string]*ir.StructType{
				"P": {Name: "P", Fields: []ir.Param{{Name: "x", BaseType: "int"}}},
			},
			Stages: map[string]*ir.Stage{
				"MAPOUT": {Name: "MAPOUT", Out: []ir.Param{{Name: "m", BaseType: "P", MapDim: 1}}},
			},
			Pipelines: map[string]*ir.Pipeline{
				"T": {
					Name: "T",
					Calls: []ir.Call{
						{Name: "ARRFORK", Callable: "MAPOUT", Mapped: true, MapMode: "array"},
						{Name: "MAPFORK", Callable: "MAPOUT", Mapped: true, MapMode: "map"},
					},
					Returns: []ir.Binding{{Param: "r", Value: ir.Value{Ref: ref}}},
				},
			},
		}
	}

	// array<map<P>>.x (array fork over a map<P> output) is now supported.
	if err := checkSupported(mk(&ir.Ref{Kind: "call", ID: "ARRFORK", Output: "m.x"})); err != nil {
		t.Errorf("checkSupported rejected array<map<S>>.field, want accepted: %v", err)
	}

	// map<map<P>>.x (map fork over a map<P> output) is a nested-map projection.
	err := checkSupported(mk(&ir.Ref{Kind: "call", ID: "MAPFORK", Output: "m.x"}))
	if err == nil {
		t.Fatal("checkSupported accepted nested map<map<S>>.field projection")
	}
	if !errors.Is(err, apperror.ErrUnsupported) {
		t.Errorf("error = %v, want ErrUnsupported", err)
	}
}

// TestMapProjectDepth covers the typed-map field-projection resolver: the depth
// at which projection begins and whether the projection descends an array
// (array<map<S>>.field), plus the edge cases a code review surfaced (an
// unresolvable ref returns 0 without panicking; an array fork over a map<S>
// output without a field access is not a projection).
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
			{Name: "MAPFORK2", Callable: "MAPOUT", Mapped: true, MapMode: "map"},
		},
	}

	cases := []struct {
		name    string
		ref     *ir.Ref
		want    int
		wantArr bool
	}{
		{"declared map<S>.field", &ir.Ref{Kind: "call", ID: "MO", Output: "m.x"}, 1, false},
		{"self map<S>.field", &ir.Ref{Kind: "self", ID: "mm", Output: "x"}, 1, false},
		{"map-fork struct .field", &ir.Ref{Kind: "call", ID: "MAPFORK", Output: "p.x"}, 1, false},
		{"array-of-map<S> field proj (supported, inArray)", &ir.Ref{Kind: "call", ID: "ARRFORK", Output: "m.x"}, 1, true},
		{"nested map<map<S>> field proj (unsupported)", &ir.Ref{Kind: "call", ID: "MAPFORK2", Output: "m.x"}, -1, false},
		{"array-fork over map<S>, no field proj", &ir.Ref{Kind: "call", ID: "ARRFORK", Output: "m"}, 0, false},
		{"plain scalar self ref", &ir.Ref{Kind: "self", ID: "n"}, 0, false},
		{"unknown call (no panic)", &ir.Ref{Kind: "call", ID: "MISSING", Output: "a.b"}, 0, false},
		{"unknown self input (no panic)", &ir.Ref{Kind: "self", ID: "absent", Output: "a.b"}, 0, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, arr := mapProjectDepth(prog, p, tc.ref)
			if got != tc.want || arr != tc.wantArr {
				t.Errorf("mapProjectDepth = (%d, %v), want (%d, %v)", got, arr, tc.want, tc.wantArr)
			}
		})
	}
}
