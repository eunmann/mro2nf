package emit

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/eunmann/mro2nf/internal/apperror"
	"github.com/eunmann/mro2nf/internal/bind"
	"github.com/eunmann/mro2nf/internal/ir"
)

// mappedWholeRefProg builds pipeline T with an array-forked and a map-forked
// call of stage MAKEB (outs), plus a plain call of it, returning ret.
func mappedWholeRefProg(outs []ir.Param, ret *ir.Ref) *ir.Program {
	return &ir.Program{
		Structs: map[string]*ir.StructType{},
		Stages:  map[string]*ir.Stage{"MAKEB": {Name: "MAKEB", Out: outs}},
		Pipelines: map[string]*ir.Pipeline{
			"T": {
				Name: "T",
				Calls: []ir.Call{
					{Name: "ARR", Callable: "MAKEB", Mapped: true, MapMode: ir.MapModeArray},
					{Name: "MAP", Callable: "MAKEB", Mapped: true, MapMode: ir.MapModeMap},
					{Name: "ONE", Callable: "MAKEB"},
				},
				Returns: []ir.Binding{{Param: "r", Value: ir.Value{Ref: ret}}},
			},
		},
	}
}

// TestValueToEntryMarksWholeMappedRef checks the emitter tags a whole-call ref
// to a mapped call with its fork kind (#240), so the binder resolves it per
// fork; a projection of a mapped call and a whole ref to a plain call stay
// untagged.
func TestValueToEntryMarksWholeMappedRef(t *testing.T) {
	outs := []ir.Param{{Name: "a", BaseType: "int"}, {Name: "n", BaseType: "int"}}

	for _, tc := range []struct {
		name string
		ref  ir.Ref
		want string
	}{
		{"whole array fork", ir.Ref{Kind: ir.RefKindCall, ID: "ARR"}, bind.ForkArray},
		{"whole map fork", ir.Ref{Kind: ir.RefKindCall, ID: "MAP"}, bind.ForkMap},
		{"projected array fork", ir.Ref{Kind: ir.RefKindCall, ID: "ARR", Output: "n"}, ""},
		{"whole plain call", ir.Ref{Kind: ir.RefKindCall, ID: "ONE"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ref := tc.ref
			prog := mappedWholeRefProg(outs, &ref)

			entry := valueToEntry(prog, prog.Pipelines["T"], ir.Value{Ref: &ref})
			if entry.Ref == nil || entry.Ref.Fork != tc.want {
				t.Errorf("entry ref = %+v, want fork %q", entry.Ref, tc.want)
			}
		})
	}
}

// TestCheckSupportedRejectsWholeRefToOutputlessMappedCall: a mapped call with no
// outputs merges to {}, which cannot say how many forks ran, so a whole ref to
// it is refused at transpile time rather than bound as a wrong-length array.
func TestCheckSupportedRejectsWholeRefToOutputlessMappedCall(t *testing.T) {
	prog := mappedWholeRefProg(nil, &ir.Ref{Kind: ir.RefKindCall, ID: "ARR"})
	if err := checkSupported(prog); !errors.Is(err, apperror.ErrUnsupported) {
		t.Errorf("checkSupported = %v, want ErrUnsupported", err)
	}
}

// TestValueToEntryEmptyComposites guards bug 3: an empty array/object literal
// must survive the bindspec JSON round-trip as an empty composite, not collapse
// to null. Encoding it as a non-nil Array/Object Entry triggers omitempty on
// marshal, so the reloaded all-nil Entry resolves to "null" (bind.go), which
// broke e.g. DETECT_CHEMISTRY's custom_chemistry_defs. Martian round-trips
// empty []/{} as empty composites (core/resolve.go, argument_map.go).
func TestValueToEntryEmptyComposites(t *testing.T) {
	cases := []struct {
		name string
		val  ir.Value
		want string
	}{
		{"empty array", ir.Value{Array: []ir.Value{}}, "[]"},
		{"empty object", ir.Value{Object: map[string]ir.Value{}}, "{}"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry := valueToEntry(nil, nil, tc.val)

			// Round-trip the bindspec through JSON exactly as emit -> mre does.
			raw, err := json.Marshal(bind.Spec{"x": entry})
			if err != nil {
				t.Fatalf("marshal spec: %v", err)
			}

			var reloaded bind.Spec
			if err := json.Unmarshal(raw, &reloaded); err != nil {
				t.Fatalf("unmarshal spec: %v", err)
			}

			got, err := bind.Resolve(reloaded, nil, nil)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if string(got) != `{"x":`+tc.want+`}` {
				t.Errorf("resolved args = %s, want x=%s (not null)", got, tc.want)
			}
		})
	}
}
