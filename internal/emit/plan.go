package emit

import "github.com/eunmann/mro2nf/internal/ir"

// featureSet is the opt-in emission flags, mirrored from Options. Grouping them
// keeps genCtx's runtime config (mre/shell/…) separate from behavior toggles and
// gives the plan a single place to read policy from.
type featureSet struct {
	fuseChains bool
}

// callKind is how a single call is emitted — decided once in buildPlan so the
// process, wiring, and include sites can never disagree (a mismatch would emit a
// dangling or missing process). The variants are evaluated in this order.
type callKind uint8

const (
	kindPlainBind     callKind = iota // standalone BIND + callee (or disable gate)
	kindMapped                        // FORK → callee → MERGE
	kindForward                       // #14 emit-once: route producer bundle straight in
	kindFusedStage                    // #16 fused bind+main leaf stage
	kindFusedSplit                    // #16 fused bind+split → MAIN → JOIN
	kindFusedDisabled                 // #59 fused bind+main, natively-gated disable
	kindFusedChain                    // #59 Lever 4 chain consumer (folds its producer)
	kindFusedAway                     // #59 Lever 4 chain producer folded into its consumer
)

// callPlan is the decided emission strategy for one call plus the analysis payload
// each site needs, so nothing is recomputed downstream.
type callPlan struct {
	kind callKind
	// stage is the callee stage for the fused-stage/split/disabled kinds.
	stage *ir.Stage
	// fwd is the producer call name for kindForward.
	fwd string
	// prod/prodStage/consStage describe a kindFusedChain fusion.
	prod      ir.Call
	prodStage *ir.Stage
	consStage *ir.Stage
	// disableTask reports that a mapped/plain disabled call needs a standalone
	// DISABLE process (the flag is not driver-gateable).
	disableTask bool
}

// pipePlan is the per-pipeline emission plan: one callPlan per call, plus whether
// the return is a pure forward (no return BIND).
type pipePlan struct {
	calls  map[string]callPlan
	retFwd string // forward producer for the return, "" when it needs a BIND
}

// emitPlan is the whole-program emission plan, computed once in buildPlan.
type emitPlan struct {
	keyed map[string]bool
	pipes map[string]pipePlan
}

// buildPlan resolves every per-call emission decision up front so the process,
// wiring, and include emitters read a fixed plan instead of re-running the
// fuseable*/forward/chain predicates at each site (the class of drift that
// produces dangling processes).
func buildPlan(prog *ir.Program, f featureSet) emitPlan {
	pl := emitPlan{keyed: keyedReachable(prog), pipes: map[string]pipePlan{}}

	for name, p := range prog.Pipelines {
		away := fusedAwayProducers(p, prog, f.fuseChains)
		pp := pipePlan{calls: make(map[string]callPlan, len(p.Calls))}

		for _, c := range p.Calls {
			pp.calls[c.Name] = planCall(c, p, prog, f, away)
		}

		if prod, ok := forwardProducer(p.Returns, p, prog); ok {
			pp.retFwd = prod
		}

		pl.pipes[name] = pp
	}

	return pl
}

// planCall decides one call's kind in the same precedence the emitters used to
// inline: chain fold, chain consumer, mapped, forward, fused stage/split/disabled,
// else a plain bind.
func planCall(c ir.Call, p *ir.Pipeline, prog *ir.Program, f featureSet, away map[string]bool) callPlan {
	if f.fuseChains {
		if away[c.Name] {
			return callPlan{kind: kindFusedAway}
		}

		if prod, ps, cs, ok := chainFusion(c, p, prog, f.fuseChains); ok {
			return callPlan{kind: kindFusedChain, prod: prod, prodStage: ps, consStage: cs}
		}
	}

	if c.Mapped {
		return callPlan{kind: kindMapped, disableTask: needsDisableTask(c)}
	}

	if prod, ok := callForwardProducer(c, p, prog); ok {
		return callPlan{kind: kindForward, fwd: prod}
	}

	if s, ok := fuseableStageCall(c, p, prog); ok {
		return callPlan{kind: kindFusedStage, stage: s}
	}

	if s, ok := fuseableSplitCall(c, p, prog); ok {
		return callPlan{kind: kindFusedSplit, stage: s}
	}

	if s, ok := fuseableDisabledStage(c, p, prog); ok {
		return callPlan{kind: kindFusedDisabled, stage: s}
	}

	return callPlan{kind: kindPlainBind, disableTask: needsDisableTask(c)}
}

// needsDisableTask reports whether a disabled call requires a standalone DISABLE
// bind (its flag is not a single top-level field readable on the driver).
func needsDisableTask(c ir.Call) bool {
	if c.Disabled == nil {
		return false
	}

	_, _, native := nativeDisableGate(c)

	return !native
}

// fusedInclude reports whether a call's module include is suppressed — the fused
// stage/chain kinds are self-contained per-call processes with no wf_ import.
func (cp callPlan) fusedInclude() bool {
	return cp.kind == kindFusedStage || cp.kind == kindFusedChain || cp.kind == kindFusedAway
}
