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

// nestedMapProg builds a program whose MAPFORK call wraps a map<P> output in a
// second map dimension (an unsupported projection source), with hook points
// for a disabled-ref or composite return binding.
func nestedMapProg(ret ir.Value, disabled *ir.Ref) *ir.Program {
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
					{Name: "MAPFORK", Callable: "MAPOUT", Mapped: true, MapMode: "map"},
					{Name: "GATED", Callable: "MAPOUT", Disabled: disabled},
				},
				Returns: []ir.Binding{{Param: "r", Value: ret}},
			},
		},
	}
}

// TestCheckSupportedDisabledRef checks a `disabled = REF` condition navigating
// an unsupported nested typed-map projection fails the transpile like a
// binding would (the disabled path through checkValueRefs).
func TestCheckSupportedDisabledRef(t *testing.T) {
	bad := &ir.Ref{Kind: "call", ID: "MAPFORK", Output: "m.x"}
	prog := nestedMapProg(ir.Value{Literal: []byte("1")}, bad)

	if err := checkSupported(prog); !errors.Is(err, apperror.ErrUnsupported) {
		t.Errorf("disabled-ref nested-map projection: want ErrUnsupported, got %v", err)
	}
}

// TestCheckSupportedCompositeRefs checks refs nested inside array/object
// binding literals reach the projection guard (a fan-in [MAPFORK.m.x] must be
// rejected exactly like a bare ref).
func TestCheckSupportedCompositeRefs(t *testing.T) {
	bad := ir.Value{Ref: &ir.Ref{Kind: "call", ID: "MAPFORK", Output: "m.x"}}

	for name, v := range map[string]ir.Value{
		"array":  {Array: []ir.Value{bad}},
		"object": {Object: map[string]ir.Value{"k": bad}},
	} {
		if err := checkSupported(nestedMapProg(v, nil)); !errors.Is(err, apperror.ErrUnsupported) {
			t.Errorf("%s-nested ref: want ErrUnsupported, got %v", name, err)
		}
	}
}

// TestMapProjectDepthArrayShapes covers the projectionShape arms with no prior
// direct case: a field beneath a plain array auto-projects (depth 0), and maps
// nested inside an array reject (negative depth).
func TestMapProjectDepthArrayShapes(t *testing.T) {
	prog := &ir.Program{
		Structs: map[string]*ir.StructType{
			"P": {Name: "P", Fields: []ir.Param{{Name: "x", BaseType: "int"}}},
		},
		Stages: map[string]*ir.Stage{
			"S": {Name: "S", Out: []ir.Param{
				{Name: "arr", BaseType: "P", ArrayDim: 1},
				{Name: "mm", BaseType: "P", MapDim: 2},
			}},
		},
	}
	p := &ir.Pipeline{Name: "T", Calls: []ir.Call{
		{Name: "PLAIN", Callable: "S"},
		{Name: "ARRFORK", Callable: "S", Mapped: true, MapMode: "array"},
	}}

	if d, inArr := mapProjectDepth(prog, p, &ir.Ref{Kind: "call", ID: "PLAIN", Output: "arr.x"}); d != 0 || inArr {
		t.Errorf("array-of-struct: got (%d,%v), want (0,false)", d, inArr)
	}

	if d, _ := mapProjectDepth(prog, p, &ir.Ref{Kind: "call", ID: "ARRFORK", Output: "mm.x"}); d >= 0 {
		t.Errorf("array over nested maps: got depth %d, want negative (unsupported)", d)
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
