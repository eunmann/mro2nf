package emit

import (
	"testing"

	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/google/go-cmp/cmp"
)

// TestDisableBindingsTopLevel pins the DISABLE bind to the gate's top-level
// value and the reader to the rest of the path (#253): the binder projects a
// null struct to a null gate, which Mro2nf now reads as enabled, while mrp
// errors on a gate path through a null struct; Mro2nf.disabledDir walking the
// path keeps that error.
func TestDisableBindingsTopLevel(t *testing.T) {
	cases := []struct {
		name     string
		gate     ir.Ref
		wantRef  ir.Ref
		wantPath string
	}{
		{
			"call field",
			ir.Ref{Kind: ir.RefKindCall, ID: "UP", Output: "on"},
			ir.Ref{Kind: ir.RefKindCall, ID: "UP", Output: "on"},
			"disabled",
		},
		{
			"call struct path",
			ir.Ref{Kind: ir.RefKindCall, ID: "UP", Output: "cfg.opts.flag"},
			ir.Ref{Kind: ir.RefKindCall, ID: "UP", Output: "cfg"},
			"disabled.opts.flag",
		},
		{
			"self struct path",
			ir.Ref{Kind: ir.RefKindSelf, ID: "cfg", Output: "flag"},
			ir.Ref{Kind: ir.RefKindSelf, ID: "cfg"},
			"disabled.flag",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gate := tc.gate
			c := ir.Call{Name: "WORK", Disabled: &gate}

			want := []ir.Binding{{Param: "disabled", Value: ir.Value{Ref: &tc.wantRef}}}
			if diff := cmp.Diff(want, disableBindings(c)); diff != "" {
				t.Errorf("disableBindings mismatch (-want +got):\n%s", diff)
			}

			if got := disablePath(c); got != tc.wantPath {
				t.Errorf("disablePath = %q, want %q", got, tc.wantPath)
			}

			if diff := cmp.Diff(tc.gate, *c.Disabled); diff != "" {
				t.Errorf("call's gate ref was modified (-want +got):\n%s", diff)
			}
		})
	}
}
