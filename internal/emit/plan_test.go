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
	if on["USE"].kind != kindFusedChain || on["USE"].prod.Name != "SRC" {
		t.Errorf("with -fuse-chains: USE kind = %d prod = %q, want kindFusedChain folding SRC", on["USE"].kind, on["USE"].prod.Name)
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

func keysOf(m map[string]pipePlan) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}

	return ks
}
