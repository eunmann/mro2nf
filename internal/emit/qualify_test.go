package emit

import (
	"slices"
	"testing"

	"github.com/eunmann/mro2nf/internal/ir"
)

// TestQualifyCollisionFree checks that distinct (pipeline, call) pairs whose
// names contain the "__" separator do not collapse to the same qualified name.
func TestQualifyCollisionFree(t *testing.T) {
	a := qualify("A", "B__C")
	b := qualify("A__B", "C")

	if a == b {
		t.Errorf("qualify collision: %q == %q", a, b)
	}

	// Sanity: the common case is stable and readable.
	if got := qualify("P", "S"); got != "1_P__S" {
		t.Errorf("qualify(P,S) = %q, want 1_P__S", got)
	}
}

// TestPartitionGateablePreflight pins the gate/rest SPLIT itself (not just the
// Warnings wording): a mapped, disabled, or call-output-bound preflight must
// stay in `rest` — wrongly gating one would change the emitted wiring, an
// output-divergence risk the shared preflightUngateable helper exists to
// prevent.
func TestPartitionGateablePreflight(t *testing.T) {
	callRef := ir.Value{Ref: &ir.Ref{Kind: "call", ID: "GEN", Output: "x"}}
	selfRef := ir.Value{Ref: &ir.Ref{Kind: "self", ID: "in"}}

	calls := []ir.Call{
		{Name: "OK", Preflight: true, Bindings: []ir.Binding{{Param: "a", Value: selfRef}}},
		{Name: "MAPPED", Preflight: true, Mapped: true},
		{Name: "GATED", Preflight: true, Disabled: &ir.Ref{Kind: "self", ID: "off"}},
		{Name: "REFBOUND", Preflight: true, Bindings: []ir.Binding{{Param: "a", Value: callRef}}},
		{Name: "PLAIN"},
	}

	pre, rest := partitionGateablePreflight(calls)

	if len(pre) != 1 || pre[0].Name != "OK" {
		t.Errorf("gateable preflights = %v, want exactly [OK]", callNames(pre))
	}

	want := []string{"MAPPED", "GATED", "REFBOUND", "PLAIN"}
	if got := callNames(rest); !slices.Equal(got, want) {
		t.Errorf("rest = %v, want %v (in original order)", got, want)
	}
}

func callNames(cs []ir.Call) []string {
	out := make([]string, len(cs))
	for i := range cs {
		out[i] = cs[i].Name
	}

	return out
}
