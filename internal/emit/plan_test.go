package emit

import (
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
