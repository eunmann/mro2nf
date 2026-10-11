package emit

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/eunmann/mro2nf/internal/frontend"
	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/google/go-cmp/cmp"
)

func lowerFixture(t *testing.T, fixture string) *ir.Program {
	t.Helper()

	base := "../../testdata/" + fixture

	ast, err := frontend.Parse(base+"/pipeline.mro", []string{base}, false)
	if err != nil {
		t.Fatalf("parse %s: %v", fixture, err)
	}

	prog, err := frontend.Lower(ast, nil)
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

// TestBuildPlanLinearChainKeepsOneTaskPerStage pins that every stage of a
// linear chain runs as its own task, one per mrp job. B binds only A's whole
// output, so it is a forward: A's bundle is routed straight into B's own stage.
func TestBuildPlanLinearChainKeepsOneTaskPerStage(t *testing.T) {
	calls := buildPlan(lowerFixture(t, "chain3"), featureSet{}).pipes["P"].calls

	for name, want := range map[string]callKind{"A": kindFusedStage, "B": kindForward, "C": kindFusedStage} {
		if got := calls[name].kind; got != want {
			t.Errorf("%s kind = %d, want %d", name, got, want)
		}
	}
}

// TestBuildPlanEntryBakedGateStaysRuntime pins that a `disabled = self.skip`
// gate whose input the entry call bakes true stays a driver-read runtime gate,
// so overriding the input at launch re-enables the call.
func TestBuildPlanEntryBakedGateStaysRuntime(t *testing.T) {
	prog := lowerFixture(t, "disabled_entry_skip")

	if got := buildPlan(prog, featureSet{}).pipes["P"].calls["GEN"].kind; got != kindFusedDisabled {
		t.Errorf("GEN kind = %d, want kindFusedDisabled", got)
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

// TestKnownValueShapes pins knownEval.known's leaf classification: literals
// and ANY entry self ref (whole-field or projected — self.cfg.list is equally
// invocation-known) flag; upstream refs and unbound sub-pipeline self refs do
// not.
func TestKnownValueShapes(t *testing.T) {
	entry := &ir.Pipeline{Name: "TOP", In: []ir.Param{{Name: "xs"}, {Name: "cfg"}}}
	sub := &ir.Pipeline{Name: "SUB", In: []ir.Param{{Name: "xs"}}}
	prog := &ir.Program{
		Entry:     &ir.EntryCall{Callable: "TOP"},
		Pipelines: map[string]*ir.Pipeline{"TOP": entry, "SUB": sub},
	}

	e := evalKnown(prog)
	if len(e.insts) != 1 {
		t.Fatalf("want only the entry instance, got %d", len(e.insts))
	}

	top, unbound := e.insts[0], e.instance(sub, knownFacts{})

	cases := []struct {
		name string
		in   *knownInst
		v    ir.Value
		want bool
	}{
		{"literal", top, ir.Value{Literal: []byte("[]")}, true},
		{"whole-field entry self", top, ir.Value{Ref: &ir.Ref{Kind: ir.RefKindSelf, ID: "xs"}}, true},
		{"projected entry self", top, ir.Value{Ref: &ir.Ref{Kind: ir.RefKindSelf, ID: "cfg", Output: "list"}}, true},
		{"unbound sub-pipeline self", unbound, ir.Value{Ref: &ir.Ref{Kind: ir.RefKindSelf, ID: "xs"}}, false},
		{"upstream ref", top, ir.Value{Ref: &ir.Ref{Kind: ir.RefKindCall, ID: "UP", Output: "xs"}}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := e.known(tc.in, tc.v, true); got != tc.want {
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

	prog, err := frontend.Lower(ast, nil)
	if err != nil {
		t.Fatalf("lower inline mro: %v", err)
	}

	return prog
}

// TestKnownInvocationConservative pins the widen-only guardrails of the #127
// propagation — the shapes that must NOT be marked, because marking them
// would turn a RUNTIME-empty fork into null where mrp merges the typed empty.
func TestKnownInvocationConservative(t *testing.T) {
	// Until SplitKnownContexts gives each site its own copy, a pipeline
	// instantiated at several sites shares one plan, so its split is marked
	// only when EVERY instance marks it: S2 binds a stage output, which must
	// unmark SUB.SC for S1 too.
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

	// A disable gate adds no knowledge: a gated producer over a RUNTIME
	// source leaves its consumer unmarked, as it would ungated.
	dis := lowerMRO(t, `
stage GEN(out int[] xs, src py "s/gen",)
stage SC(in int v, out int w, src py "s/sc",)
pipeline P(in bool skip, out int[] a, out int[] b,){
    call GEN()
    map call SC as A(v = split GEN.xs,) using (disabled = self.skip,)
    map call SC as B(v = split A.w,)
    return (a = A.w, b = B.w,)
}
call P(skip = false,)
`)

	dk := knownInvocation(dis)
	if dk.emptySplit["P"]["A"] || dk.emptySplit["P"]["B"] {
		t.Error("A splits an upstream output; neither A nor its consumer B may be marked, gated or not")
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

	inner := instancesOf(evalKnown(proj), "INNER")
	if len(inner) != 1 || !inner[0].facts.len["ps"] || inner[0].facts.val["ps"] {
		t.Fatal("test premise: INNER.ps must be length-known only (cascade-bound)")
	}

	if knownInvocation(proj).emptySplit["INNER"]["SC"] {
		t.Error("a projected split of a lenIn-only input must not be marked (value knowledge required)")
	}
}

// instancesOf returns the evaluated instances of the named pipeline.
func instancesOf(e *knownEval, name string) []*knownInst {
	var out []*knownInst

	for _, in := range e.insts {
		if in.p.Name == name {
			out = append(out, in)
		}
	}

	return out
}

// calleeOf returns the callable a pipeline's call names.
func calleeOf(t *testing.T, prog *ir.Program, pipe, call string) string {
	t.Helper()

	c, ok := callByName(prog.Pipelines[pipe], call)
	if !ok {
		t.Fatalf("%s has no call %s", pipe, call)
	}

	return c.Callable
}

// TestEmptyNullPerCallSite pins #241: empty_fork_two_sites calls SUB from an
// invocation-known empty (S1, mrp: null) and a runtime empty (S2, mrp: []),
// both disable-gated so inlining keeps the boundaries. Each site gets its own
// copy of SUB, and only S1's copy merges zero forks to null — in every mode,
// with and without inlining (split first, as Emit does).
func TestEmptyNullPerCallSite(t *testing.T) {
	for _, f := range []featureSet{{}, {native: true}, {inlinePipelines: true}, {native: true, inlinePipelines: true}} {
		prog := lowerFixture(t, "empty_fork_two_sites")
		SplitKnownContexts(prog)

		if f.inlinePipelines {
			inlinePipelines(prog)
		}

		s1, s2 := calleeOf(t, prog, "TOP", "S1"), calleeOf(t, prog, "TOP", "S2")
		if s1 != "SUB" || s2 != "SUB_ctx2" {
			t.Fatalf("%+v: S1 calls %s and S2 calls %s, want SUB and SUB_ctx2", f, s1, s2)
		}

		pl := buildPlan(prog, f)
		if !pl.pipes[s1].calls["SCALE"].emptyNull {
			t.Errorf("%+v: S1's SUB splits an invocation-known empty; its merge must give null", f)
		}

		if pl.pipes[s2].calls["SCALE"].emptyNull {
			t.Errorf("%+v: S2's SUB splits a runtime empty; its merge must keep the typed empty", f)
		}
	}
}

// TestEmptyNullThroughDisableGate pins #383: a map call splitting the output
// of a disable-gated producer whose own split source is invocation-known is
// marked whatever the gate, because mrp folds a literal gate statically and
// prunes the producer's known-empty split to null under a runtime one (a
// runtime gate over a value that is not statically null is a binding error
// in mrp). The two fixtures cover a gated map call, a gated sub-pipeline that
// maps, and a gated pass-through sub-pipeline, under literal and runtime
// gates — in every mode, with and without inlining.
func TestEmptyNullThroughDisableGate(t *testing.T) {
	cases := []struct {
		fixture, pipe string
		calls         []string
	}{
		{"empty_fork_gated", "EFG", []string{"A_LIT", "B_LIT", "A_RT", "B_RT", "A_RTON", "B_RTON", "A_OFF", "B_OFF"}},
		{"empty_fork_gated_sub", "EFGS", []string{"B_S_LIT", "B_S_RT", "B_S_RTON", "B_S_OFF", "B_P_LIT", "B_P_OFF"}},
	}

	for _, tc := range cases {
		for _, f := range []featureSet{{}, {native: true}, {inlinePipelines: true}, {native: true, inlinePipelines: true}} {
			prog := lowerFixture(t, tc.fixture)
			SplitKnownContexts(prog)

			if f.inlinePipelines {
				inlinePipelines(prog)
			}

			pl := buildPlan(prog, f)
			for _, call := range tc.calls {
				if !pl.pipes[tc.pipe].calls[call].emptyNull {
					t.Errorf("%s %+v: %s must merge zero forks to null", tc.fixture, f, call)
				}
			}
		}
	}
}

// TestSplitKnownContexts pins the split itself: copies propagate down the call
// tree (MID's two contexts give SUB two), a context difference no plan reads
// clones nothing, and the input program survives a split of a shallow copy
// (internal/overrides relies on that).
func TestSplitKnownContexts(t *testing.T) {
	src := `
stage GEN(out int[] xs, out string tag, src py "s/gen",)
stage SC(in int v, in string tag, out int w, src py "s/sc",)
pipeline SUB(in int[] vals, in string tag, out int[] w,){
    map call SC(v = split self.vals, tag = self.tag,)
    return (w = SC.w,)
}
pipeline MID(in int[] xs, in string tag, out int[] w,){
    call SUB(vals = self.xs, tag = self.tag,)
    return (w = SUB.w,)
}
pipeline TOP(in int[] empty, in string tag, out int[] a, out int[] b, out int[] c,){
    call GEN()
    call MID as M1(xs = self.empty, tag = self.tag,)
    call MID as M2(xs = GEN.xs, tag = self.tag,)
    call MID as M3(xs = self.empty, tag = GEN.tag,)
    return (a = M1.w, b = M2.w, c = M3.w,)
}
call TOP(empty = [], tag = "t",)
`
	orig := lowerMRO(t, src)
	before := maps.Clone(orig.Pipelines)

	prog := *orig
	prog.Pipelines = maps.Clone(orig.Pipelines)
	SplitKnownContexts(&prog)

	got := map[string]string{}
	for _, call := range []string{"M1", "M2", "M3"} {
		mid := calleeOf(t, &prog, "TOP", call)
		got[call] = mid + ">" + calleeOf(t, &prog, mid, "SUB")
	}

	want := map[string]string{"M1": "MID>SUB", "M2": "MID_ctx2>SUB_ctx2", "M3": "MID>SUB"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("call targets (-want +got):\n%s", diff)
	}

	k := knownInvocation(&prog)
	if !k.emptySplit["SUB"]["SC"] || k.emptySplit["SUB_ctx2"]["SC"] {
		t.Errorf("SUB.SC must be marked and SUB_ctx2.SC not: %v", k.emptySplit)
	}

	if len(orig.Pipelines) != len(before) || calleeOf(t, orig, "TOP", "M2") != "MID" {
		t.Error("splitting a shallow copy must leave the input program untouched")
	}

	for name, p := range before {
		if orig.Pipelines[name] != p {
			t.Errorf("input pipeline %s was replaced", name)
		}
	}
}

// TestSplitKnownContextsNoop pins that a program whose call sites agree is
// left exactly as it was (no copies, the same pipeline values), so existing
// projects emit unchanged.
func TestSplitKnownContextsNoop(t *testing.T) {
	for _, fixture := range []string{"empty_fork_sub", "fork_disabled_sub", "cellranger_shaped", "kitchen_sink", "map_pipe_nested"} {
		prog := lowerFixture(t, fixture)
		before := maps.Clone(prog.Pipelines)

		SplitKnownContexts(prog)

		if len(prog.Pipelines) != len(before) {
			t.Errorf("%s: split added pipelines", fixture)
		}

		for name, p := range before {
			if prog.Pipelines[name] != p {
				t.Errorf("%s: pipeline %s was replaced", fixture, name)
			}
		}
	}
}

// TestReturnKnownPerCallSite pins that a ref THROUGH a sub-pipeline call reads
// that call's own instance: PASS forwards its input, so `split P1.ys` is
// invocation-known while `split P2.ys` is not — mrp gives a: null, b: [] for
// this program (checked against mrp). PASS plans the same under both sites, so
// nothing is copied.
func TestReturnKnownPerCallSite(t *testing.T) {
	prog := lowerMRO(t, `
stage GEN(out int[] xs, src py "s/gen",)
stage SC(in int v, out int w, src py "s/sc",)
pipeline PASS(in int[] xs, out int[] ys,){
    return (ys = self.xs,)
}
pipeline TOP(in int[] empty, out int[] a, out int[] b,){
    call GEN()
    call PASS as P1(xs = self.empty,)
    call PASS as P2(xs = GEN.xs,)
    map call SC as SA(v = split P1.ys,)
    map call SC as SB(v = split P2.ys,)
    return (a = SA.w, b = SB.w,)
}
call TOP(empty = [],)
`)
	SplitKnownContexts(prog)

	if _, ok := prog.Pipelines["PASS_ctx2"]; ok {
		t.Error("PASS plans identically at both sites; it must not be copied")
	}

	k := knownInvocation(prog)
	if !k.emptySplit["TOP"]["SA"] || k.emptySplit["TOP"]["SB"] {
		t.Errorf("SA (through P1) must be marked and SB (through P2) not: %v", k.emptySplit["TOP"])
	}
}

// TestBuildPlanForward guards #73's forward planning: file_chain's READFILE
// just forwards MAKEFILE.f, so it routes MAKEFILE's bundle straight in.
func TestBuildPlanForward(t *testing.T) {
	fc := lowerFixture(t, "file_chain")

	if got := buildPlan(fc, featureSet{}).pipes["CP"].calls["READFILE"].kind; got != kindForward {
		t.Errorf("READFILE kind = %d, want kindForward", got)
	}
}

func keysOf(m map[string]pipePlan) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}

	return ks
}

// String names keyedKind values so test failures read as constants, not raw
// numbers. Test-scoped on purpose: nothing reachable from the binaries prints
// the enum, so a production method would trip the deadcode gate.
func (k keyedKind) String() string {
	switch k {
	case keyedBind:
		return "keyedBind"
	case keyedFused:
		return "keyedFused"
	case keyedForkBind:
		return "keyedForkBind"
	case keyedScatter:
		return "keyedScatter"
	default:
		return fmt.Sprintf("keyedKind(%d)", uint8(k))
	}
}

// TestKeyedScatterableRejections pins keyedScatterable's guardrails (#99): the
// keyed driver scatter requires a value-only callee and no call-ref bindings.
// map_pipe_nested is the positive control (keyedScatter over xs); a
// file-bearing callee input (map_pipe_nested_file) and a broadcast binding
// referencing an upstream call must both fall back to keyedForkBind — the _KS
// forkbind assembles fargs without the type manifest or a per-outer-key
// producer join, so either shape would break on the scatter path. A rejection
// must also leave the scatter payload empty (no field, no stage): a stale
// payload beside keyedForkBind would let an emit site scatter anyway.
func TestKeyedScatterableRejections(t *testing.T) {
	el := buildPlan(lowerFixture(t, "map_pipe_nested"), featureSet{}).pipes["INNER"].calls["DBL"]
	if el.keyedKind != keyedScatter || el.keyedScatterField != "xs" {
		t.Errorf("value-only nested map: keyedKind = %v (field %q), want keyedScatter over xs",
			el.keyedKind, el.keyedScatterField)
	}

	fb := buildPlan(lowerFixture(t, "map_pipe_nested_file"), featureSet{}).pipes["INNER"].calls["DBL"]
	if fb.keyedKind != keyedForkBind || fb.keyedScatterField != "" || fb.keyedStage != nil {
		t.Errorf("file-bearing callee input: keyedKind = %v (field %q, stage %v), want bare keyedForkBind (file-leaf rejection)",
			fb.keyedKind, fb.keyedScatterField, fb.keyedStage)
	}

	cr := lowerMRO(t, `
stage GEN(out int k, src py "s/gen",)
stage DBL(in int x, in int k, out int y, src py "s/dbl",)
pipeline INNER(in int[] xs, out int[] ys,){
    call GEN()
    map call DBL(x = split self.xs, k = GEN.k,)
    return (ys = DBL.y,)
}
pipeline OUTER(in int[][] sets, out int[][] yss,){
    map call INNER(xs = split self.sets,)
    return (yss = INNER.ys,)
}
call OUTER(sets = [[1]],)
`)
	got := buildPlan(cr, featureSet{}).pipes["INNER"].calls["DBL"]
	if got.keyedKind != keyedForkBind || got.keyedScatterField != "" || got.keyedStage != nil {
		t.Errorf("call-ref broadcast binding: keyedKind = %v (field %q, stage %v), want bare keyedForkBind (call-ref rejection)",
			got.keyedKind, got.keyedScatterField, got.keyedStage)
	}
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
		{native: true, nativeRunner: true},
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
