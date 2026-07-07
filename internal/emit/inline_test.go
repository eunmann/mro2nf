package emit

import (
	"testing"

	"github.com/eunmann/mro2nf/internal/ir"
)

// prog builds a two-level program: pipeline TOP calls stage GEN then plain
// sub-pipeline SUB(x = GEN.out); SUB calls stage USE(in = self.x) and returns
// out = USE.out. Inlining TOP must splice USE as TOP's call "S_USE" reading
// GEN.out directly, and TOP's return must resolve to S_USE.out.
func inlineFixtureProg() *ir.Program {
	gen := &ir.Stage{Name: "GEN"}
	use := &ir.Stage{Name: "USE"}

	sub := &ir.Pipeline{
		Name: "SUB",
		In:   []ir.Param{{Name: "x"}},
		Out:  []ir.Param{{Name: "out"}},
		Calls: []ir.Call{{
			Name: "USE", Callable: "USE",
			Bindings: []ir.Binding{{Param: "in", Value: ir.Value{Ref: &ir.Ref{Kind: ir.RefKindSelf, ID: "x"}}}},
		}},
		Returns: []ir.Binding{{Param: "out", Value: ir.Value{Ref: &ir.Ref{Kind: ir.RefKindCall, ID: "USE", Output: "out"}}}},
	}

	top := &ir.Pipeline{
		Name: "TOP",
		Out:  []ir.Param{{Name: "y"}},
		Calls: []ir.Call{
			{Name: "GEN", Callable: "GEN"},
			{
				Name: "S", Callable: "SUB",
				Bindings: []ir.Binding{{Param: "x", Value: ir.Value{Ref: &ir.Ref{Kind: ir.RefKindCall, ID: "GEN", Output: "out"}}}},
			},
		},
		Returns: []ir.Binding{{Param: "y", Value: ir.Value{Ref: &ir.Ref{Kind: ir.RefKindCall, ID: "S", Output: "out"}}}},
	}

	return &ir.Program{
		Stages:    map[string]*ir.Stage{"GEN": gen, "USE": use},
		Pipelines: map[string]*ir.Pipeline{"TOP": top, "SUB": sub},
		Structs:   map[string]*ir.StructType{},
		Entry:     &ir.EntryCall{Callable: "TOP"},
	}
}

func TestInlineSplicesAndRewrites(t *testing.T) {
	prog := inlineFixtureProg()
	inlinePipelines(prog)

	top := prog.Pipelines["TOP"]
	if len(top.Calls) != 2 {
		t.Fatalf("TOP should have 2 calls after inlining (GEN + spliced USE), got %d: %+v", len(top.Calls), inlineCallNames(top.Calls))
	}

	// The sub-pipeline call "S" is gone; its USE is spliced as "S_USE".
	su := inlineFindCall(top.Calls, "S_USE")
	if su == nil {
		t.Fatalf("expected spliced call S_USE, got %v", inlineCallNames(top.Calls))
	}

	if inlineFindCall(top.Calls, "S") != nil {
		t.Errorf("the sub-pipeline boundary call S must be gone after inlining")
	}

	// S_USE's `in` (was self.x) must now read GEN.out directly.
	inRef := su.Bindings[0].Value.Ref
	if inRef == nil || inRef.Kind != ir.RefKindCall || inRef.ID != "GEN" || inRef.Output != "out" {
		t.Errorf("S_USE.in should resolve self.x -> GEN.out, got %+v", su.Bindings[0].Value)
	}

	// TOP's return (was S.out) must now resolve to S_USE.out.
	retRef := top.Returns[0].Value.Ref
	if retRef == nil || retRef.ID != "S_USE" || retRef.Output != "out" {
		t.Errorf("TOP return should resolve S.out -> S_USE.out, got %+v", top.Returns[0].Value)
	}
}

// TestInlineSkipsMappedAndDisabled checks the conservative gate: a mapped or
// disabled sub-pipeline call keeps its boundary (not inlined).
func TestInlineSkipsMappedAndDisabled(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*ir.Call)
	}{
		{"mapped", func(c *ir.Call) { c.Mapped = true }},
		{"disabled", func(c *ir.Call) { c.Disabled = &ir.Ref{Kind: ir.RefKindSelf, ID: "off"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prog := inlineFixtureProg()
			for i := range prog.Pipelines["TOP"].Calls {
				if prog.Pipelines["TOP"].Calls[i].Name == "S" {
					tc.mutate(&prog.Pipelines["TOP"].Calls[i])
				}
			}

			inlinePipelines(prog)

			if inlineFindCall(prog.Pipelines["TOP"].Calls, "S") == nil {
				t.Errorf("a %s sub-pipeline call must stay a boundary (not inlined)", tc.name)
			}
		})
	}
}

func inlineFindCall(cs []ir.Call, name string) *ir.Call {
	for i := range cs {
		if cs[i].Name == name {
			return &cs[i]
		}
	}

	return nil
}

func inlineCallNames(cs []ir.Call) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Name
	}

	return out
}
