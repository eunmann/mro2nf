package emit

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/eunmann/mro2nf/internal/frontend"
	"github.com/eunmann/mro2nf/internal/ir"
)

func lowerFixture(t *testing.T, fixture string) *ir.Program {
	t.Helper()

	base := "../../testdata/" + fixture

	ast, err := frontend.Parse(base+"/pipeline.mro", []string{base}, false)
	if err != nil {
		t.Fatalf("parse %s: %v", fixture, err)
	}

	prog, err := frontend.Lower(ast)
	if err != nil {
		t.Fatalf("lower %s: %v", fixture, err)
	}

	return prog
}

// TestBuildPlan pins the centralized emission decisions (#77): every call's kind
// is resolved once so the process/wiring/include sites can't disagree. kitchen_sink
// exercises the fused-leaf (preflight), fused-disabled, fused-split, and mapped
// kinds in one pipeline.
func TestBuildPlan(t *testing.T) {
	prog := lowerFixture(t, "kitchen_sink")
	pl := buildPlan(prog, featureSet{})

	want := map[string]callKind{
		"CHECK":       kindFusedStage,    // preflight leaf fuses (#59)
		"STATS":       kindFusedDisabled, // self.skip_stats disable, gated natively
		"SUM_SQUARES": kindFusedSplit,    // split stage
		"SCALE_ALL":   kindPlainBind,     // plain sub-pipeline call (bind for its args)
	}

	main, ok := pl.pipes["MAIN"]
	if !ok {
		t.Fatalf("no plan for pipeline MAIN; pipelines=%v", keysOf(pl.pipes))
	}

	for call, kind := range want {
		if got := main.calls[call].kind; got != kind {
			t.Errorf("plan MAIN.%s kind = %d, want %d", call, got, kind)
		}
	}

	// The map call lives inside the SCALE_ALL sub-pipeline, not at MAIN.
	if got := pl.pipes["SCALE_ALL"].calls["SCALE"].kind; got != kindMapped {
		t.Errorf("plan SCALE_ALL.SCALE kind = %d, want kindMapped", got)
	}
}

// TestBuildPlanChainFusion pins the -fuse-chains decisions: the source folds away
// and its consumer becomes the chain process; without the flag both are plain
// fused stages.
func TestBuildPlanChainFusion(t *testing.T) {
	prog := lowerFixture(t, "chain_fuse")

	off := buildPlan(prog, featureSet{}).pipes["CH"].calls
	if off["SRC"].kind != kindFusedStage || off["USE"].kind != kindFusedStage {
		t.Errorf("without -fuse-chains: SRC=%d USE=%d, want both fused stages", off["SRC"].kind, off["USE"].kind)
	}

	on := buildPlan(prog, featureSet{fuseChains: true}).pipes["CH"].calls
	if on["SRC"].kind != kindFusedAway {
		t.Errorf("with -fuse-chains: SRC kind = %d, want kindFusedAway", on["SRC"].kind)
	}
	if on["USE"].kind != kindFusedChain || on["USE"].chain[0].call.Name != "SRC" {
		t.Errorf("with -fuse-chains: USE kind = %d, want kindFusedChain with SRC first", on["USE"].kind)
	}
}

// TestBuildPlanFoldDisables pins #59 Lever 1: with -fold-disables an entry
// `disabled = self.skip` gate whose input the entry bakes true folds the call to
// kindFoldedOff (pruned to its null output); without the flag it stays a normal
// natively-gated disabled leaf. A disable that is NOT entry-determinable (an
// upstream CALL.out ref) never folds, even with the flag on.
func TestBuildPlanFoldDisables(t *testing.T) {
	fd := lowerFixture(t, "fold_disable")

	if got := buildPlan(fd, featureSet{}).pipes["P"].calls["GEN"].kind; got != kindFusedDisabled {
		t.Errorf("without -fold-disables: GEN kind = %d, want kindFusedDisabled", got)
	}
	if got := buildPlan(fd, featureSet{foldDisables: true}).pipes["P"].calls["GEN"].kind; got != kindFoldedOff {
		t.Errorf("with -fold-disables: GEN kind = %d, want kindFoldedOff", got)
	}

	// disabled_callref's WORK is gated on an upstream output (FLAG.on) — runtime-
	// derived, so it must NOT fold even with the flag on.
	cr := lowerFixture(t, "disabled_callref")
	if got := buildPlan(cr, featureSet{foldDisables: true}).pipes["DC"].calls["WORK"].kind; got == kindFoldedOff {
		t.Errorf("WORK gated on FLAG.on must not fold (runtime-derived), got kindFoldedOff")
	}
}

// TestBuildPlanEmptyNull pins the #99 empty-fork fidelity rule: a map call
// whose split source is launch-invocation-known (a whole-field entry self ref
// or a literal) gets emptyNull — its ZERO-fork merge emits null, matching
// mrp's static resolver pruning a statically-empty fork — in default and
// native mode alike. A map call splitting an UPSTREAM output must NOT get it:
// a runtime empty merges to the typed empty. The flag is shape-based (not
// value-based), so launch-time entry overrides stay correct: override the
// input to non-empty and the forks run, override to empty and mrp with that
// invocation would produce null too.
func TestBuildPlanEmptyNull(t *testing.T) {
	for _, fx := range []struct{ fixture, pipe, call string }{
		{"empty_fork_min", "EF", "SCALE"},
		{"empty_map_fork", "EMP", "DBL"},
		{"fork_min", "SCALE_ALL", "SCALE"}, // non-empty entry source: flagged too (never fires at runtime)
		// #127 value-chain propagation (knownInvocation): a sub-pipeline split
		// chained to an entry value through the parent call's bindings, the
		// in-pipeline cascade through a mapped call's output, and a MIXED
		// entry+upstream zip must all be flagged — mrp's static resolver
		// prunes each to null when the entry side is empty.
		{"empty_fork_sub", "SUB", "SCALE"},
		{"empty_fork_cascade", "EFC", "FIRST"},
		{"empty_fork_cascade", "EFC", "SECOND"},
		{"empty_fork_mixed", "EFM", "PAIR"},
	} {
		prog := lowerFixture(t, fx.fixture)

		for _, f := range []featureSet{{}, {native: true}} {
			if !buildPlan(prog, f).pipes[fx.pipe].calls[fx.call].emptyNull {
				t.Errorf("%s (native=%v): entry-sourced split must set emptyNull", fx.fixture, f.native)
			}
		}
	}

	// Upstream-sourced splits stay typed-empty: zero forks is a RUNTIME fact.
	ref := lowerFixture(t, "runtime_empty_forks")
	for _, call := range []string{"SC_EA", "SC_NA", "SC_EM", "SC_NM"} {
		if buildPlan(ref, featureSet{native: true}).pipes["REF"].calls[call].emptyNull {
			t.Errorf("%s splits an upstream output; a runtime empty must keep the typed empty", call)
		}
	}
}

// TestKnownValueShapes pins invKnown.known's leaf classification: literals and
// ANY entry self ref (whole-field or projected — self.cfg.list is equally
// invocation-known) flag; upstream refs and unbound sub-pipeline self refs do
// not.
func TestKnownValueShapes(t *testing.T) {
	entry := &ir.Pipeline{Name: "TOP"}
	sub := &ir.Pipeline{Name: "SUB"}
	prog := &ir.Program{
		Entry:     &ir.EntryCall{Callable: "TOP"},
		Pipelines: map[string]*ir.Pipeline{"TOP": entry, "SUB": sub},
	}
	k := knownInvocation(prog)

	cases := []struct {
		name string
		p    *ir.Pipeline
		v    ir.Value
		want bool
	}{
		{"literal", entry, ir.Value{Literal: []byte("[]")}, true},
		{"whole-field entry self", entry, ir.Value{Ref: &ir.Ref{Kind: ir.RefKindSelf, ID: "xs"}}, true},
		{"projected entry self", entry, ir.Value{Ref: &ir.Ref{Kind: ir.RefKindSelf, ID: "cfg", Output: "list"}}, true},
		{"unbound sub-pipeline self", sub, ir.Value{Ref: &ir.Ref{Kind: ir.RefKindSelf, ID: "xs"}}, false},
		{"upstream ref", entry, ir.Value{Ref: &ir.Ref{Kind: ir.RefKindCall, ID: "UP", Output: "xs"}}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := k.known(prog, tc.p, tc.v, true); got != tc.want {
				t.Errorf("known(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// lowerMRO parses and lowers an inline .mro source (src paths unchecked), for
// pinning analysis shapes no committed fixture needs to run end-to-end.
func lowerMRO(t *testing.T, src string) *ir.Program {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.mro")

	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatalf("write mro: %v", err)
	}

	ast, err := frontend.Parse(path, []string{dir}, false)
	if err != nil {
		t.Fatalf("parse inline mro: %v", err)
	}

	prog, err := frontend.Lower(ast)
	if err != nil {
		t.Fatalf("lower inline mro: %v", err)
	}

	return prog
}

// TestKnownInvocationConservative pins the widen-only guardrails of the #127
// propagation — the shapes that must NOT be marked, because marking them
// would turn a RUNTIME-empty fork into null where mrp merges the typed empty.
func TestKnownInvocationConservative(t *testing.T) {
	// A pipeline instantiated at several sites shares one plan, so an input is
	// invocation-known only when EVERY site binds it so: S2 binds a stage
	// output, which must unmark SUB.SC for S1 too.
	multi := lowerMRO(t, `
stage GEN(out int[] xs, src py "s/gen",)
stage SC(in int v, out int w, src py "s/sc",)
pipeline SUB(in int[] vals, out int[] w,){
    map call SC(v = split self.vals,)
    return (w = SC.w,)
}
pipeline TOP(in int[] values, out int[] a, out int[] b,){
    call GEN()
    call SUB as S1(vals = self.values,)
    call SUB as S2(vals = GEN.xs,)
    return (a = S1.w, b = S2.w,)
}
call TOP(values = [],)
`)
	if knownInvocation(multi).emptySplit["SUB"]["SC"] {
		t.Error("SUB is also instantiated with a runtime collection (S2); its split must not be marked")
	}

	// A cascade source has invocation-known LENGTH but runtime element values,
	// so a map-called sub-pipeline fed its ELEMENTS must not mark an inner
	// split (mrp resolves those elements at runtime); the outer scatter over
	// the cascade output itself IS marked.
	elem := lowerMRO(t, `
stage DUP(in int[] xs, out int[] ys, src py "s/dup",)
stage SC(in int v, out int w, src py "s/sc",)
pipeline INNER(in int[] items, out int[] w,){
    map call SC(v = split self.items,)
    return (w = SC.w,)
}
pipeline TOP(in int[][] grid, out int[][] w,){
    map call DUP(xs = split self.grid,)
    map call INNER(items = split DUP.ys,)
    return (w = INNER.w,)
}
call TOP(grid = [],)
`)

	ek := knownInvocation(elem)
	if !ek.emptySplit["TOP"]["DUP"] || !ek.emptySplit["TOP"]["INNER"] {
		t.Error("entry split and its cascade consumer must both be marked")
	}

	if ek.emptySplit["INNER"]["SC"] {
		t.Error("INNER's elements are runtime values (DUP outputs); its inner split must not be marked")
	}

	// A DISABLED producer is never known: its gate may null it at runtime,
	// where mrp's typed-empty rule applies — the consumer must stay unmarked.
	dis := lowerMRO(t, `
stage SC(in int v, out int w, src py "s/sc",)
pipeline P(in int[] xs, in bool skip, out int[] a, out int[] b,){
    map call SC as A(v = split self.xs,) using (disabled = self.skip,)
    map call SC as B(v = split A.w,)
    return (a = A.w, b = B.w,)
}
call P(xs = [], skip = false,)
`)

	dk := knownInvocation(dis)
	if !dk.emptySplit["P"]["A"] {
		t.Error("A splits an entry source; its own merge is marked even when disable-gated")
	}

	if dk.emptySplit["P"]["B"] {
		t.Error("B splits a disabled call's output — runtime-nullable, must not be marked")
	}

	// A PROJECTED split of a length-only-known input must not mark: INNER.ps
	// is lenIn-only (bound to a cascade collection — invocation-known LENGTH,
	// runtime values), and length knowledge of the container proves nothing
	// about a projected field, so `split self.ps.arr` requires VALUE
	// knowledge. The guard is structural (invKnown.known), not a per-type
	// argument about which projections happen to preserve the outer length.
	proj := lowerMRO(t, `
struct Pair(int[] arr,)
stage MK(in int x, out Pair p, src py "s/mk",)
stage SC(in int[] v, out int w, src py "s/sc",)
pipeline INNER(in Pair[] ps, out int[] w,){
    map call SC(v = split self.ps.arr,)
    return (w = SC.w,)
}
pipeline TOP(in int[] xs, out int[] w,){
    map call MK(x = split self.xs,)
    call INNER(ps = MK.p,)
    return (w = INNER.w,)
}
call TOP(xs = [],)
`)

	pk := knownInvocation(proj)
	if !pk.lenIn["INNER"]["ps"] || pk.valIn["INNER"]["ps"] {
		t.Fatal("test premise: INNER.ps must be lenIn-only (cascade-bound)")
	}

	if pk.emptySplit["INNER"]["SC"] {
		t.Error("a projected split of a lenIn-only input must not be marked (value knowledge required)")
	}
}

// TestBuildPlanForwardChain guards #73: a source feeding a pure-FORWARD consumer
// folds under -fuse-chains too. file_chain's MAKEFILE feeds READFILE, which just
// forwards MAKEFILE.f — so with the flag MAKEFILE folds away and READFILE becomes
// the chain process; without it, READFILE stays a plain forward.
func TestBuildPlanForwardChain(t *testing.T) {
	fc := lowerFixture(t, "file_chain")

	if got := buildPlan(fc, featureSet{}).pipes["CP"].calls["READFILE"].kind; got != kindForward {
		t.Errorf("without -fuse-chains: READFILE kind = %d, want kindForward", got)
	}

	on := buildPlan(fc, featureSet{fuseChains: true}).pipes["CP"].calls
	if on["MAKEFILE"].kind != kindFusedAway {
		t.Errorf("with -fuse-chains: MAKEFILE kind = %d, want kindFusedAway", on["MAKEFILE"].kind)
	}
	if on["READFILE"].kind != kindFusedChain || on["READFILE"].chain[0].call.Name != "MAKEFILE" {
		t.Errorf("with -fuse-chains: READFILE kind = %d, want kindFusedChain with MAKEFILE first", on["READFILE"].kind)
	}
}

// TestBuildPlanNStageChain guards #81: a 3-stage linear run A->B->C folds into
// one chain ending at C (all three links) with A and B folded away.
func TestBuildPlanNStageChain(t *testing.T) {
	c3 := lowerFixture(t, "chain_fuse3")
	on := buildPlan(c3, featureSet{fuseChains: true}).pipes["P"].calls

	if on["A"].kind != kindFusedAway || on["B"].kind != kindFusedAway {
		t.Errorf("A/B should fold away: A=%d B=%d", on["A"].kind, on["B"].kind)
	}

	cp := on["C"]
	if cp.kind != kindFusedChain || len(cp.chain) != 3 {
		t.Fatalf("C kind = %d, chain len = %d, want kindFusedChain of 3", cp.kind, len(cp.chain))
	}
	if cp.chain[0].call.Name != "A" || cp.chain[1].call.Name != "B" || cp.chain[2].call.Name != "C" {
		t.Errorf("chain order = %s,%s,%s, want A,B,C", cp.chain[0].call.Name, cp.chain[1].call.Name, cp.chain[2].call.Name)
	}
}

func keysOf(m map[string]pipePlan) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}

	return ks
}

// TestPlanKeyedKindFlagInvariant pins planKeyedCall's documented featureSet
// independence: the keyed decision (and payload) must be identical under every
// flag combination, since the keyed layer's correctness for flag-composed
// runs rests on it. If a future change threads a flag into the keyed
// predicates, this forces the composed golden coverage question to be asked.
func TestPlanKeyedKindFlagInvariant(t *testing.T) {
	sets := []featureSet{
		{},
		{native: true},
		{fuseChains: true, foldDisables: true, native: true, nativeRunner: true},
	}

	for _, fixture := range []string{"map_pipe_nested", "map_pipe_disabled_nested", "map_pipe_split", "kitchen_sink"} {
		prog := lowerFixture(t, fixture)

		base := buildPlan(prog, sets[0])

		for _, f := range sets[1:] {
			pl := buildPlan(prog, f)

			for name, pp := range base.pipes {
				for call, cp := range pp.calls {
					got := pl.pipes[name].calls[call]
					if got.keyedKind != cp.keyedKind || got.keyedScatterField != cp.keyedScatterField ||
						got.keyedDisableTask != cp.keyedDisableTask || got.keyedStage != cp.keyedStage {
						t.Errorf("%s %s.%s: keyed decision varies with featureSet %+v", fixture, name, call, f)
					}
				}
			}
		}
	}
}
