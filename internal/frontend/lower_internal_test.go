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

// TestLowerCallDisabledNonRef checks an unexpected `disabled =` expression
// shape (anything but a reference) fails loudly with ErrUnsupported instead of
// silently lowering the call to "always enabled".
func TestLowerCallDisabledNonRef(t *testing.T) {
	c := &syntax.CallStm{Id: "X", DecId: "X", Modifiers: &syntax.Modifiers{
		Bindings: &syntax.BindStms{
			Table: map[string]*syntax.BindStm{
				"disabled": {Id: "disabled", Exp: &syntax.BoolExp{Value: true}},
			},
		},
	}}

	if _, err := lowerCall(c); !errors.Is(err, apperror.ErrUnsupported) {
		t.Errorf("lowerCall(disabled = literal): want ErrUnsupported, got %v", err)
	}
}

// TestDisabledRefAbsent checks the no-modifier shapes still lower to a nil
// disable gate without error.
func TestDisabledRefAbsent(t *testing.T) {
	for name, m := range map[string]*syntax.Modifiers{
		"no bindings":       {},
		"no disabled entry": {Bindings: &syntax.BindStms{Table: map[string]*syntax.BindStm{}}},
	} {
		ref, err := disabledRef(m)
		if err != nil {
			t.Errorf("disabledRef(%s): unexpected error %v", name, err)
		}

		if ref != nil {
			t.Errorf("disabledRef(%s) = %+v, want nil", name, ref)
		}
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
