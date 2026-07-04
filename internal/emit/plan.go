package emit

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/eunmann/mro2nf/internal/ir"
)

// featureSet is the opt-in emission flags, mirrored from Options. Grouping them
// keeps genCtx's runtime config (mre/shell/…) separate from behavior toggles and
// gives the plan a single place to read policy from.
type featureSet struct {
	fuseChains   bool
	foldDisables bool
	native       bool
	// nativeRunner swaps the Python stage-execution hop from the Martian
	// adapter (mre + martian_shell.py) to the embedded direct-call runner (#79).
	nativeRunner bool
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
	// scatterField is the whole-field name whose collection a kindNativeScatter
	// call forks over — the driver reads its size from pipeargs' data.json (self
	// source) or the producer's bundle (upstream source; see scatterCall).
	scatterField string
	// scatterCall is the producing call whose value-channel bundle a
	// kindNativeScatter reads its fork width from, or "" for a self source.
	scatterCall string
	// scatterValueOnly marks a kindNativeScatter whose split element carries no
	// file leaves, so the driver slices the collection once and hands each fork
	// its own element (O(1) per instance) instead of every instance re-parsing
	// the whole collection via forkbind -index (the O(N^2) fix, #99).
	scatterValueOnly bool
	// foldMerge marks a kindNativeScatter call whose MERGE gather runs inline in
	// its sole consumer's task (#76): no standalone MERGE process; the consumer
	// stages the per-fork outs dirs + keys sidecar and reconstructs merged_<id>
	// with `mre merge` before its own bind.
	foldMerge bool
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
	pl := emitPlan{pipes: map[string]pipePlan{}}
	queued := queuePipeArgs(prog)

	for name, p := range prog.Pipelines {
		away := fusedAwayProducers(p, prog, f.fuseChains)
		pp := pipePlan{calls: make(map[string]callPlan, len(p.Calls))}

		for _, c := range p.Calls {
			pp.calls[c.Name] = planCall(c, p, prog, f, away, queued[name])
		}

		if prod, ok := forwardProducer(p.Returns, p, prog); ok {
			pp.retFwd = prod
		}

		// Second pass (#76 merge fold): with every call's kind fixed, decide which
		// scatters fold their MERGE into the sole consumer. Reading the finished
		// kinds keeps this a plan decision the emitters can't disagree with (#77).
		for _, c := range p.Calls {
			if cp := pp.calls[c.Name]; cp.kind == kindNativeScatter && mergeFoldable(c.Name, p, pp) {
				cp.foldMerge = true
				pp.calls[c.Name] = cp
			}
		}

		pl.pipes[name] = pp
	}

	// keyedReachable reads the per-call kinds, so the pipes must be complete
	// first (planCall reads nothing from the keyed set — no cycle).
	pl.keyed = keyedReachable(prog, pl)
	pl.modules = neededStageModules(prog, pl)

	return pl
}

// queuePipeArgs returns the pipelines whose plain workflow can receive a QUEUE
// channel as its pipeargs instead of the usual value channel: a disabled call
// hands the callee its gated run-branch (a 0/1-item queue), and queue-ness
// propagates down the plain call tree (a bind fed a queue emits a queue). A
// native scatter with upstream-ref inputs would zip its N-item fork channel
// against those 1-item queues and run a single fork, so eligibility consults
// this set (#76). Mapped calls are excluded — a map-called pipeline runs its
// keyed layer, not the plain workflow.
func queuePipeArgs(prog *ir.Program) map[string]bool {
	queued := map[string]bool{}

	// Iterate to a fixed point: the call graph is a DAG pipeline-wise, but a
	// caller's queue-ness may be decided after its callees were visited.
	for changed := true; changed; {
		changed = false

		for name, p := range prog.Pipelines {
			for _, c := range p.Calls {
				if _, ok := prog.Pipelines[c.Callable]; !ok || c.Mapped || queued[c.Callable] {
					continue
				}

				if c.Disabled != nil || queued[name] {
					queued[c.Callable] = true
					changed = true
				}
			}
		}
	}

	return queued
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
func planCall(c ir.Call, p *ir.Pipeline, prog *ir.Program, f featureSet, away map[string]bool, queuedPa bool) callPlan {
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
			if s, field, call, ok := nativeScatterable(c, prog, queuedPa); ok {
				// The O(1) element path pulls pipeargs out as a broadcast input, so
				// it needs a VALUE-channel pipeargs; in a queue-pipeargs context
				// (a disabled sub-pipeline callee) it would zip N forks down to one,
				// so those keep the pipeargs-in-tuple path.
				return callPlan{
					kind: kindNativeScatter, stage: s, scatterField: field, scatterCall: call,
					scatterValueOnly: !queuedPa && splitValueOnly(c, s, prog),
				}
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
// with no FORK task (#76), returning the callee stage, the whole-field name
// whose collection sizes the scatter, and (for an upstream-ref source) the
// producing call whose channel the driver reads that width from ("" for a self
// source). This increment covers a non-split leaf stage callee, no disable
// gate, no preflight, and exactly one split binding that is a whole-field ref:
// either a self input (`split self.field`, width read from pipeargs' data.json)
// or a top-level upstream output (`split CALL.field`, width read from the
// producer's value-channel bundle). A projection (`self.a.b` / `CALL.a.b`) can
// navigate a typed map and is left to the FORK path. Wildcard bindings never
// carry Split (the Martian grammar has no `* = split ...` production), so they
// fall through the !b.Split skip. In a queue-pipeargs pipeline, upstream-ref
// bindings are disqualifying: the refs' 1-item queue channels would zip against
// the N-item fork channel and run a single fork. Ineligible map calls keep the
// FORK path — still correct, just not collapsed.
func nativeScatterable(c ir.Call, prog *ir.Program, queuedPa bool) (*ir.Stage, string, string, bool) {
	if c.Disabled != nil || c.Preflight {
		return nil, "", "", false
	}

	s, ok := prog.Stages[c.Callable]
	if !ok || s.Split {
		return nil, "", "", false
	}

	if queuedPa && len(refCalls(c.Bindings)) > 0 {
		return nil, "", "", false
	}

	field, call, splits := "", "", 0

	for _, b := range c.Bindings {
		if !b.Split {
			continue
		}

		splits++

		f, cl, ok := scatterSource(b.Value.Ref)
		if !ok {
			return nil, "", "", false
		}

		field, call = f, cl
	}

	if splits != 1 {
		return nil, "", "", false
	}

	return s, field, call, true
}

// splitValueOnly reports whether a native scatter's split element carries no
// file leaves — the callee's parameter bound by the split binding is a
// file-free type. Only then can the driver slice the collection into plain JSON
// elements (no bundle file markers to rewrite per fork), enabling the O(1)
// forkbind -elementfile path; a file-bearing split keeps the -index path.
func splitValueOnly(c ir.Call, s *ir.Stage, prog *ir.Program) bool {
	for _, b := range c.Bindings {
		if !b.Split {
			continue
		}

		for i := range s.In {
			if s.In[i].Name == b.Param {
				return !hasFileLeaf(s.In[i], prog.Structs)
			}
		}
	}

	return false
}

// scatterSource classifies a split binding's ref as a driver-readable scatter
// source, returning the width field, the producing call ("" for a self source),
// and whether it qualifies. Eligible: a whole-field self input (`self.field`)
// or a whole top-level upstream output (`CALL.field`). A projection (a dotted
// Output) can navigate a typed map and is left to the FORK path.
func scatterSource(r *ir.Ref) (string, string, bool) {
	switch {
	case r == nil:
		return "", "", false
	case r.Kind == refKindSelf && r.Output == "":
		return r.ID, "", true
	case r.Kind == refKindCall && r.Output != "" && !strings.Contains(r.Output, "."):
		return r.Output, r.ID, true
	default:
		return "", "", false
	}
}

// mergeFoldable reports whether a native scatter's MERGE can run inline in its
// consumer's task (#76): exactly one consumer (mirroring the #59 Lever 4
// single-consumer rule — K consumers would duplicate the merge and stage the N
// fork dirs K times), and that consumer is a task-hosted bind shape — the
// pipeline return (return BIND, or the native LAYOUT for the entry) or a
// plain/fused/mapped call. A disable-gate reference is never foldable (the
// driver reads that bundle directly; no task hosts the merge) — consumerCount
// counts gate refs, so a gate-referenced producer either has 2+ consumers or
// its sole "consumer" is the gate, which matches neither the returns nor any
// call bindings below and falls through to false. A downstream scatter
// consumer keeps the MERGE: folding there would re-merge once per fork
// instance. Forward/chain consumers cannot reference a mapped producer
// (forwardProducer and chainFusion both reject them). Dormancy invariant: a
// folded consumer must stage BOTH the souts and keys channels — souts emits []
// even for a skipped pipeline; only the unbound keys channel keeps the
// consumer dormant (see genNativeScatterWiring).
func mergeFoldable(name string, p *ir.Pipeline, pp pipePlan) bool {
	if consumerCount(name, p) != 1 {
		return false
	}

	if slices.Contains(refCalls(p.Returns), name) {
		return true
	}

	for _, c := range p.Calls {
		if !slices.Contains(refCalls(c.Bindings), name) {
			continue
		}

		switch pp.calls[c.Name].kind {
		case kindPlainBind, kindFusedStage, kindFusedDisabled, kindMapped:
			return true
		default:
			return false
		}
	}

	return false
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
