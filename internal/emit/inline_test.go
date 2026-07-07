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

// TestInlineSkipsMapped checks the conservative gate: a mapped sub-pipeline call
// keeps its boundary (not inlined) — its per-fork output shape is not expressible
// by splicing.
func TestInlineSkipsMapped(t *testing.T) {
	prog := inlineFixtureProg()
	for i := range prog.Pipelines["TOP"].Calls {
		if prog.Pipelines["TOP"].Calls[i].Name == "S" {
			prog.Pipelines["TOP"].Calls[i].Mapped = true
		}
	}

	inlinePipelines(prog)

	if inlineFindCall(prog.Pipelines["TOP"].Calls, "S") == nil {
		t.Errorf("a mapped sub-pipeline call must stay a boundary (not inlined)")
	}
}

// TestInlineDisabledRuntimePushesGate checks that a plain sub-pipeline call
// disabled by a runtime ref IS inlined, with the outer gate pushed onto every
// spliced internal call so the sub skips (and nulls) exactly when the gate fires.
func TestInlineDisabledRuntimePushesGate(t *testing.T) {
	prog := inlineFixtureProg()
	gate := &ir.Ref{Kind: ir.RefKindSelf, ID: "off"}
	setCallDisable(prog.Pipelines["TOP"].Calls, "S", gate)

	inlinePipelines(prog)

	top := prog.Pipelines["TOP"]
	if inlineFindCall(top.Calls, "S") != nil {
		t.Fatalf("a runtime-disabled plain sub-pipeline call must be inlined, got %v", inlineCallNames(top.Calls))
	}

	su := inlineFindCall(top.Calls, "S_USE")
	if su == nil {
		t.Fatalf("expected spliced call S_USE, got %v", inlineCallNames(top.Calls))
	}

	if su.Disabled == nil || su.Disabled.Kind != ir.RefKindSelf || su.Disabled.ID != "off" {
		t.Errorf("S_USE must inherit the outer gate self.off, got %+v", su.Disabled)
	}

	// TOP's return (was S.out) still resolves to S_USE.out — which nulls with the
	// now-disabled S_USE, matching mrp nulling the disabled sub's output.
	retRef := top.Returns[0].Value.Ref
	if retRef == nil || retRef.ID != "S_USE" || retRef.Output != "out" {
		t.Errorf("TOP return should resolve S.out -> S_USE.out, got %+v", top.Returns[0].Value)
	}
}

// TestInlineDisabledKeepsBoundary checks the safety gate: a runtime-disabled sub
// is kept as a boundary when the outer gate cannot be pushed byte-identically —
// an internal call with its own disable (needs X OR Y), a literal return leaf, or
// a self-ref return leaf (neither nulls when the sub is disabled).
func TestInlineDisabledKeepsBoundary(t *testing.T) {
	gate := &ir.Ref{Kind: ir.RefKindSelf, ID: "off"}

	for _, tc := range []struct {
		name   string
		mutate func(*ir.Pipeline)
	}{
		{"internal own disable", func(s *ir.Pipeline) {
			s.Calls[0].Disabled = &ir.Ref{Kind: ir.RefKindSelf, ID: "inner"}
		}},
		{"literal return leaf", func(s *ir.Pipeline) {
			s.Returns[0].Value = ir.Value{Literal: []byte("7")}
		}},
		{"self-ref return leaf", func(s *ir.Pipeline) {
			s.Returns[0].Value = ir.Value{Ref: &ir.Ref{Kind: ir.RefKindSelf, ID: "x"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prog := inlineFixtureProg()
			setCallDisable(prog.Pipelines["TOP"].Calls, "S", gate)
			tc.mutate(prog.Pipelines["SUB"])

			inlinePipelines(prog)

			if inlineFindCall(prog.Pipelines["TOP"].Calls, "S") == nil {
				t.Errorf("an unsafe runtime-disabled sub (%s) must stay a boundary", tc.name)
			}
		})
	}
}

// TestInlineDisabledKeepsBoundaryUnprofitable checks the overhead gate: a
// runtime-disabled sub whose internal calls the emitter cannot fuse into gated
// binds stays a boundary, because inlining it would run those binds
// unconditionally and cost MORE tasks than the single short-circuiting boundary
// bind — a split internal stage, an internally-chained call, and a mapped
// internal call each keep the boundary.
func TestInlineDisabledKeepsBoundaryUnprofitable(t *testing.T) {
	gate := &ir.Ref{Kind: ir.RefKindSelf, ID: "off"}

	for _, tc := range []struct {
		name   string
		mutate func(*ir.Program)
	}{
		{"split internal stage", func(p *ir.Program) { p.Stages["USE"].Split = true }},
		{"mapped internal call", func(p *ir.Program) { p.Pipelines["SUB"].Calls[0].Mapped = true }},
		{"internally chained call", func(p *ir.Program) {
			sub := p.Pipelines["SUB"]
			sub.Calls = append(sub.Calls, ir.Call{
				Name: "USE2", Callable: "USE",
				Bindings: []ir.Binding{{Param: "in", Value: ir.Value{Ref: &ir.Ref{Kind: ir.RefKindCall, ID: "USE", Output: "out"}}}},
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prog := inlineFixtureProg()
			setCallDisable(prog.Pipelines["TOP"].Calls, "S", gate)
			tc.mutate(prog)

			inlinePipelines(prog)

			if inlineFindCall(prog.Pipelines["TOP"].Calls, "S") == nil {
				t.Errorf("an unprofitable runtime-disabled sub (%s) must stay a boundary", tc.name)
			}
		})
	}
}

// TestInlineDisabledConstantFolds checks that a disable gate resolving to a
// constant is handled by the existing fold path, not the runtime-inline path: a
// literal-false gate drops and the sub inlines undisabled; a literal-true gate
// prunes the sub to a null output.
func TestInlineDisabledConstantFolds(t *testing.T) {
	for _, tc := range []struct {
		name        string
		gateLit     string
		wantSpliced bool
	}{
		{"false gate inlines undisabled", "false", true},
		{"true gate prunes to null", "true", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prog := constGateProg(tc.gateLit)
			inlinePipelines(prog)

			top := prog.Pipelines["TOP"]
			if inlineFindCall(top.Calls, "S") != nil {
				t.Errorf("boundary call S should be gone (folded), got %v", inlineCallNames(top.Calls))
			}

			su := inlineFindCall(top.Calls, "S_USE")
			if (su != nil) != tc.wantSpliced {
				t.Errorf("S_USE present=%v, want %v (%v)", su != nil, tc.wantSpliced, inlineCallNames(top.Calls))
			}

			if su != nil && su.Disabled != nil {
				t.Errorf("a folded-false gate must leave S_USE undisabled, got %+v", su.Disabled)
			}
		})
	}
}

func setCallDisable(cs []ir.Call, name string, d *ir.Ref) {
	for i := range cs {
		if cs[i].Name == name {
			cs[i].Disabled = d
		}
	}
}

// constGateProg builds the inline fixture with the sub-pipeline call disabled by
// GATE.flag, where GATE is a call-free sub-pipeline returning the literal gateLit.
// After GATE inlines, S's gate resolves through subs to that constant, exercising
// the fold path (drop when false / prune when true) rather than runtime inline.
func constGateProg(gateLit string) *ir.Program {
	prog := inlineFixtureProg()

	prog.Pipelines["GATE"] = &ir.Pipeline{
		Name:    "GATE",
		Out:     []ir.Param{{Name: "flag"}},
		Returns: []ir.Binding{{Param: "flag", Value: ir.Value{Literal: []byte(gateLit)}}},
	}

	top := prog.Pipelines["TOP"]
	top.Calls = append([]ir.Call{{Name: "G", Callable: "GATE"}}, top.Calls...)
	setCallDisable(top.Calls, "S", &ir.Ref{Kind: ir.RefKindCall, ID: "G", Output: "flag"})

	return prog
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
