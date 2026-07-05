package frontend

import (
	"errors"
	"testing"

	"github.com/eunmann/mro2nf/internal/apperror"
	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/martian-lang/martian/martian/syntax"
)

// TestMapMode pins the explicit syntax.CallMode -> ir MapMode vocabulary: the
// three lowerable fork kinds map to the ir constants, and every other mode is
// a typed ErrUnsupported rather than an unvetted string leaking into emit.
func TestMapMode(t *testing.T) {
	cases := []struct {
		name    string
		mode    syntax.CallMode
		want    string
		wantErr bool
	}{
		{name: "array", mode: syntax.ModeArrayCall, want: ir.MapModeArray},
		{name: "map", mode: syntax.ModeMapCall, want: ir.MapModeMap},
		{name: "unknown", mode: syntax.ModeUnknownMapCall, want: ir.MapModeUnknown},
		{name: "single", mode: syntax.ModeSingleCall, wantErr: true},
		{name: "null", mode: syntax.ModeNullMapCall, wantErr: true},
		{name: "invalid", mode: syntax.CallMode(99), wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := mapMode(tc.mode)

			if tc.wantErr {
				if !errors.Is(err, apperror.ErrUnsupported) {
					t.Fatalf("mapMode(%v) error = %v, want ErrUnsupported", tc.mode, err)
				}

				return
			}

			if err != nil {
				t.Fatalf("mapMode(%v): %v", tc.mode, err)
			}

			if got != tc.want {
				t.Errorf("mapMode(%v) = %q, want %q", tc.mode, got, tc.want)
			}
		})
	}
}

// TestLowerExpUnsupported checks an expression kind with no lowering fails
// with the typed ErrUnsupported (no corpus fixture currently reaches this arm).
func TestLowerExpUnsupported(t *testing.T) {
	if _, err := lowerExp(&syntax.SplitExp{}); !errors.Is(err, apperror.ErrUnsupported) {
		t.Errorf("lowerExp(SplitExp as leaf): want ErrUnsupported, got %v", err)
	}
}

// TestDisabledRefNonRef checks a non-reference `disabled =` binding is dropped
// (nil), leaving the call unconditionally enabled — pinned so the silent drop
// stays a deliberate contract rather than an accident.
func TestDisabledRefNonRef(t *testing.T) {
	m := &syntax.Modifiers{Bindings: &syntax.BindStms{
		Table: map[string]*syntax.BindStm{
			"disabled": {Id: "disabled", Exp: &syntax.BoolExp{Value: true}},
		},
	}}

	if ref := disabledRef(m); ref != nil {
		t.Errorf("disabledRef(literal) = %+v, want nil", ref)
	}

	if ref := disabledRef(&syntax.Modifiers{}); ref != nil {
		t.Errorf("disabledRef(no bindings) = %+v, want nil", ref)
	}
}

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
