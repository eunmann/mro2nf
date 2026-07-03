package emit

import (
	"encoding/json"

	"github.com/eunmann/mro2nf/internal/ir"
)

// featureSet is the opt-in emission flags, mirrored from Options. Grouping them
// keeps genCtx's runtime config (mre/shell/…) separate from behavior toggles and
// gives the plan a single place to read policy from.
type featureSet struct {
	fuseChains   bool
	foldDisables bool
	native       bool
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
	kindFoldedOff                     // #59 Lever 1 always-disabled call: emit only its null output
	kindNativeScatter                 // #76 -native map call: in-workflow scatter, no FORK task
)

// chainLink is one stage in a fused linear chain: the call and its stage.
type chainLink struct {
	call  ir.Call
	stage *ir.Stage
}

// callPlan is the decided emission strategy for one call plus the analysis payload
// each site needs, so nothing is recomputed downstream.
type callPlan struct {
	kind callKind
	// stage is the callee stage for the fused-stage/split/disabled kinds.
	stage *ir.Stage
	// fwd is the producer call name for kindForward.
	fwd string
	// chain is the fused linear run for kindFusedChain, source-first and ending at
	// this call (length >= 2); every stage but the last folds away (#59 Lever 4).
	chain []chainLink
	// disableTask reports that a mapped/plain disabled call needs a standalone
	// DISABLE process (the flag is not driver-gateable).
	disableTask bool
	// scatterField is the pipeline-input name whose collection a kindNativeScatter
	// call forks over — the driver reads its size from pipeargs' data.json.
	scatterField string
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
	// modules is the set of stages whose stage_<name>.nf module is actually
	// referenced (imported) somewhere; a stage fused into every one of its call
	// sites has a dead module that is not emitted (#82).
	modules map[string]bool
}

// buildPlan resolves every per-call emission decision up front so the process,
// wiring, and include emitters read a fixed plan instead of re-running the
// fuseable*/forward/chain predicates at each site (the class of drift that
// produces dangling processes).
func buildPlan(prog *ir.Program, f featureSet) emitPlan {
	pl := emitPlan{keyed: keyedReachable(prog, f), pipes: map[string]pipePlan{}}

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

	pl.modules = neededStageModules(prog, pl)

	return pl
}

// neededStageModules returns the stages whose stage_<name>.nf is referenced: any
// call whose include names the stage (it is not a fully-inlined fused kind), any
// keyed-reachable stage (its fork-keyed variants live in the module), and a stage
// entry point. A stage absent from this set is fused everywhere and its module is
// dead — writeModules skips it (#82).
func neededStageModules(prog *ir.Program, pl emitPlan) map[string]bool {
	needed := map[string]bool{}

	mark := func(callable string) {
		if _, ok := prog.Stages[callable]; ok {
			needed[callable] = true
		}
	}

	for name, keyed := range pl.keyed {
		if keyed {
			mark(name)
		}
	}

	if prog.Entry != nil {
		mark(prog.Entry.Callable)
	}

	for name, p := range prog.Pipelines {
		pp := pl.pipes[name]

		for _, c := range p.Calls {
			if !pp.calls[c.Name].fusedInclude() {
				mark(c.Callable)
			}
		}
	}

	return needed
}

// planCall decides one call's kind in the same precedence the emitters used to
// inline: chain fold, chain consumer, mapped, forward, fused stage/split/disabled,
// else a plain bind.
func planCall(c ir.Call, p *ir.Pipeline, prog *ir.Program, f featureSet, away map[string]bool) callPlan {
	// #59 Lever 1: an always-disabled call (its gate constant-folds to true) needs
	// no stage or gate — only its null output, which downstream reads as it would
	// when skipped at runtime. Takes precedence over every run-path kind.
	if f.foldDisables {
		if _, ok := foldDisableOff(prog, p, c); ok {
			return callPlan{kind: kindFoldedOff}
		}
	}

	if f.fuseChains {
		if away[c.Name] {
			return callPlan{kind: kindFusedAway}
		}

		if chain, ok := chainFusion(c, p, prog, f.fuseChains); ok {
			return callPlan{kind: kindFusedChain, chain: chain}
		}
	}

	if c.Mapped {
		// #76: under -native an eligible map call scatters in-workflow (each stage
		// instance resolves its own fork via forkbind -index), so no FORK task.
		if f.native {
			if s, field, ok := nativeScatterable(c, prog); ok {
				return callPlan{kind: kindNativeScatter, stage: s, scatterField: field}
			}
		}

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

// foldDisableOff reports whether a call's disable constant-folds to true — so the
// call never runs and can be pruned to its null output. Scoped to the safe,
// unambiguous case: a `disabled = self.<input>` gate on the ENTRY pipeline whose
// input the entry call bakes as a `true` bool literal (the entry pipeline is
// instantiated once, so the value is unambiguous). It returns the entry input
// name so a diagnostic can name it. A `disabled = CALL.out.x` gate is
// runtime-derived and never folds; a `false` literal leaves the call gated.
func foldDisableOff(prog *ir.Program, p *ir.Pipeline, c ir.Call) (string, bool) {
	r := c.Disabled
	if r == nil || r.Kind != refKindSelf || r.Output != "" {
		return "", false
	}

	if prog.Entry == nil || prog.Entry.Callable != p.Name {
		return "", false
	}

	for _, b := range prog.Entry.Bindings {
		if b.Param != r.ID {
			continue
		}

		var v bool
		if b.Value.Ref == nil && json.Unmarshal(b.Value.Literal, &v) == nil && v {
			return r.ID, true
		}

		return "", false
	}

	return "", false
}

// nativeScatterable reports whether a -native map call can scatter in-workflow
// with no FORK task (#76), returning the callee stage and the pipeline-input
// field whose collection sizes the scatter. This increment covers the shape
// whose fork width the driver can read from pipeargs' data.json: a non-split
// leaf stage callee, no disable gate, and exactly one split binding that is a
// whole-field self ref (a projection or upstream ref would need the FORK task's
// full bind resolution to know the width). Ineligible map calls keep the FORK
// path — still correct, just not yet collapsed.
func nativeScatterable(c ir.Call, prog *ir.Program) (*ir.Stage, string, bool) {
	if c.Disabled != nil {
		return nil, "", false
	}

	s, ok := prog.Stages[c.Callable]
	if !ok || s.Split {
		return nil, "", false
	}

	field, splits := "", 0

	for _, b := range c.Bindings {
		if !b.Split {
			continue
		}

		splits++

		r := b.Value.Ref
		if r == nil || r.Kind != refKindSelf || r.Output != "" {
			return nil, "", false
		}

		field = r.ID
	}

	if splits != 1 {
		return nil, "", false
	}

	return s, field, true
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
// stage/chain/scatter kinds are self-contained per-call processes with no wf_
// import.
func (cp callPlan) fusedInclude() bool {
	return cp.kind == kindFusedStage || cp.kind == kindFusedChain || cp.kind == kindFusedAway ||
		cp.kind == kindFoldedOff || cp.kind == kindNativeScatter
}
