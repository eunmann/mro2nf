package emit

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/eunmann/mro2nf/internal/ir"
)

// featureSet is the opt-in emission flags, derived from Options by its
// featureSet method (the single constructor — see emit.go). Grouping them
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
	// scatterQueuedPa marks a kindNativeScatter in a queue-pipeargs pipeline (a
	// disabled sub-pipeline callee): pipeargs cannot broadcast into N element
	// instances there, so each element tuple carries the pipeargs bundle
	// (forkElementsPa) instead of the process taking pa as a broadcast input.
	scatterQueuedPa bool
	// emptyNull marks a map call whose split source is launch-invocation-known
	// (knownInvocation, #127): its ZERO-fork merge emits null instead of the
	// typed empty, matching mrp's static resolver pruning a statically-empty
	// fork (#99).
	emptyNull bool
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
	known := knownInvocation(prog)

	for name, p := range prog.Pipelines {
		away := fusedAwayProducers(p, prog, f.fuseChains)
		pp := pipePlan{calls: make(map[string]callPlan, len(p.Calls))}

		for _, c := range p.Calls {
			pp.calls[c.Name] = planCall(c, p, prog, f, away, queued[name], known)
		}

		if prod, ok := forwardProducer(p.Returns, p, prog); ok {
			pp.retFwd = prod
		}

		// Second pass (#76/#99 merge fold): with every call's kind fixed, decide
		// which map-call gathers fold their MERGE into the sole consumer. Reading
		// the finished kinds keeps this a plan decision the emitters can't
		// disagree with (#77). Under -native the fold covers EVERY kindMapped
		// target — leaf stage, split stage, or sub-pipeline — because every keyed
		// callee variant emits the tuple(key, outs__<key>) the fold contract
		// pairs with FORK.out.keys (the leaf _MAP, the split JOIN_K, and the
		// keyed pipeline's return/forward all write outs__<key>). A DISABLED
		// mapped call keeps its MERGE — the skip branch needs the merged bundle
		// as the mix point for the null output.
		for _, c := range p.Calls {
			cp := pp.calls[c.Name]

			foldable := cp.kind == kindNativeScatter ||
				(f.native && cp.kind == kindMapped && c.Disabled == nil)

			if foldable && mergeFoldable(c.Name, p, pp) {
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
func planCall(c ir.Call, p *ir.Pipeline, prog *ir.Program, f featureSet, away map[string]bool, queuedPa bool, known invKnown) callPlan {
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
		// emptyNull may mark a DISABLED map call too. Load-bearing invariant:
		// its MERGE executes only on the gate-false (run) branch — FORK feeds
		// it and FORK receives only enabled forks (genMappedWiring) — where a
		// zero-fork run means the invocation-known split side was empty and
		// null is exactly mrp's prune. The gate-true branch never reaches the
		// merge; the skip branch mixes in the null bundle instead.
		emptyNull := known.emptySplit[p.Name][c.Name]

		// #76/#99: under -native an eligible VALUE-ONLY map call scatters
		// in-workflow on the O(1) element path — the driver slices the fork
		// collection once and each fused instance assembles its args from its
		// own element; zero orchestration tasks. A file-bearing element (bundle
		// markers a JSON slice can't carry), multi-split, projection, or
		// disabled leaf call takes kindMapped instead: ONE FORK task resolves
		// every fork (O(total)), the keyed callee runs stage main per fork with
		// no per-instance bind work, and the MERGE folds into the sole consumer
		// where eligible — so no path re-parses the collection per instance.
		if f.native {
			if s, field, call, ok := nativeScatterable(c, prog, queuedPa); ok && splitValueOnly(c, s, prog) {
				return callPlan{
					kind: kindNativeScatter, stage: s, scatterField: field, scatterCall: call,
					// In a queue-pipeargs context (a disabled sub-pipeline callee)
					// pipeargs cannot broadcast to N instances, so the element
					// tuple carries it instead (forkElementsPa).
					scatterQueuedPa: queuedPa,
					emptyNull:       emptyNull,
				}
			}
		}

		return callPlan{kind: kindMapped, disableTask: needsDisableTask(c), emptyNull: emptyNull}
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

	if !entryScoped(prog, p) {
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

// invKnown is the invocation-known analysis over the IR binding graph (#127):
// per pipeline, which inputs and mapped calls the LAUNCH INVOCATION alone
// determines. Martian's resolver treats an invocation-known empty fork
// collection differently from a runtime one — observed against mrp 4.0.15: a
// static []/{} split source resolves the whole mapped call to NULL (the
// zero-fork call is pruned, and KnownLength propagation cascades the prune
// through value chains), while an upstream-produced empty or null collection
// merges to the typed empty ([] / {}). mro2nf keeps entry inputs overridable
// at launch, so the distinction is never baked statically; the analysis only
// ever WIDENS the emptyNull marking, and the marking is purely a runtime
// shape rule: the call's ZERO-fork MERGE emits null (bind.Merge emptyNull).
// Override the entry side to non-empty and the forks run and merge normally;
// override to empty and the result is null — exactly what mrp produces for
// that invocation.
type invKnown struct {
	// valIn marks pipeline inputs whose VALUE every call site binds
	// invocation-known; lenIn additionally admits mapped-call cascade sources,
	// whose LENGTH (not values) is invocation-determined — emptiness is a
	// length fact, so lenIn is what split marking consults.
	valIn map[string]map[string]bool
	lenIn map[string]map[string]bool
	// emptySplit is the widened entry-split predicate per (pipeline, map
	// call): some split binding's emptiness is invocation-determined, so a
	// zero-fork run implies mrp's resolver pruned this call to null.
	emptySplit map[string]map[string]bool
}

// knownInvocation computes invKnown by fixed-point iteration (the same scheme
// as queuePipeArgs): the pipeline call graph is a DAG, but a caller's binding
// knowledge may be decided after its callees were visited. All three
// predicates are monotone (no rule negates another), so iterating to no
// change yields the least fixed point — conservative: anything not provably
// invocation-determined keeps the runtime typed-empty merge.
func knownInvocation(prog *ir.Program) invKnown {
	k := invKnown{
		valIn:      make(map[string]map[string]bool, len(prog.Pipelines)),
		lenIn:      make(map[string]map[string]bool, len(prog.Pipelines)),
		emptySplit: make(map[string]map[string]bool, len(prog.Pipelines)),
	}

	for name := range prog.Pipelines {
		k.valIn[name] = map[string]bool{}
		k.lenIn[name] = map[string]bool{}
		k.emptySplit[name] = map[string]bool{}
	}

	for changed := true; changed; {
		changed = false

		for name, p := range prog.Pipelines {
			for i := range p.In {
				in := p.In[i].Name
				if !k.valIn[name][in] && k.inputKnown(prog, name, in, false) {
					k.valIn[name][in] = true
					changed = true
				}

				if !k.lenIn[name][in] && k.inputKnown(prog, name, in, true) {
					k.lenIn[name][in] = true
					changed = true
				}
			}

			for _, c := range p.Calls {
				if c.Mapped && !k.emptySplit[name][c.Name] && k.splitKnown(prog, p, c) {
					k.emptySplit[name][c.Name] = true
					changed = true
				}
			}
		}
	}

	return k
}

// inputKnown reports whether the named pipeline input is invocation-known at
// EVERY call site — one pipePlan serves all instantiations of a pipeline, so
// the marking must hold for each of them (a never-called pipeline gets
// nothing). A split-bound input receives ELEMENTS of the caller's collection,
// so only a fully value-known collection qualifies even for lenOnly: a
// cascade source has invocation-known length but runtime element values. An
// input some site leaves unbound (or binds via a wildcard) stays unknown.
func (k invKnown) inputKnown(prog *ir.Program, name, in string, lenOnly bool) bool {
	found := false

	for _, q := range prog.Pipelines {
		for _, c := range q.Calls {
			if c.Callable != name {
				continue
			}

			b, ok := bindingFor(c, in)
			if !ok || !k.known(prog, q, b.Value, lenOnly && !b.Split) {
				return false
			}

			found = true
		}
	}

	return found
}

// splitKnown is the per-call marking rule: ANY split binding whose emptiness
// is invocation-known flags the map call. ANY (not ALL) is sound for a MIXED
// zip: zipped splits must agree in length (bind fails loudly with errSplitLen
// on a mismatch), so a ZERO-fork run implies every side — the known side
// included — resolved empty, which is exactly the invocation whose statically
// visible 0-length makes mrp prune the call to null.
func (k invKnown) splitKnown(prog *ir.Program, p *ir.Pipeline, c ir.Call) bool {
	for _, b := range c.Bindings {
		if b.Split && k.known(prog, p, b.Value, true) {
			return true
		}
	}

	return false
}

// known reports whether a binding value in pipeline p is invocation-known.
// With lenOnly=false the VALUE must be: a ref-free literal (an empty literal
// split is a Martian parse error, so literals only ever plumb non-split
// bindings), ANY self ref on the entry pipeline (whole-field or projected —
// entry inputs come only from the launch invocation, overrides included), a
// self ref on an invocation-known input, or a plain sub-pipeline ref whose
// return chain is known. lenOnly=true relaxes value to LENGTH/emptiness
// knowledge, additionally admitting a mapped call's output: the merged
// collection's size equals the fork count, which is invocation-determined
// exactly when that call's own split source is (mrp's KnownLength cascade) —
// its zero-fork merge yields null, and any projection of null stays null. The
// whole-outs ref of a mapped call is excluded: only a named output projects
// to the per-fork collection. A composite CONTAINING refs (fan-in
// `[A.out, B.out]`) is rejected even though its static length makes the
// zero-fork case unreachable today — the guard is structural, not an unstated
// invariant. A disabled call is never known: its gate may null it at runtime,
// where mrp's typed-empty rule (not the static prune) applies. Recursion
// descends the pipeline call DAG through returnKnown (Martian forbids
// recursive pipelines), so depth is bounded by the program's nesting.
func (k invKnown) known(prog *ir.Program, p *ir.Pipeline, v ir.Value, lenOnly bool) bool {
	r := v.Ref
	if r == nil {
		return !valueHasRef(v)
	}

	if r.Kind == refKindSelf {
		if entryScoped(prog, p) {
			return true
		}

		// A PROJECTED self ref (r.Output != "") navigates INTO the input, and
		// lenIn's length-only sources (cascade collections) carry runtime
		// element VALUES — length knowledge of the container proves nothing
		// about a projected field's length. So a projection always requires
		// VALUE knowledge; only a whole-field ref may lean on lenIn. The
		// guard is structural, not a per-type argument about which
		// projections happen to preserve the outer length (same stance as
		// the composite-refs rejection above).
		if lenOnly && r.Output == "" {
			return k.lenIn[p.Name][r.ID]
		}

		return k.valIn[p.Name][r.ID]
	}

	c, ok := callByName(p, r.ID)
	if !ok || c.Disabled != nil {
		return false
	}

	if c.Mapped {
		return lenOnly && r.Output != "" && k.emptySplit[p.Name][c.Name]
	}

	sub, ok := prog.Pipelines[c.Callable]
	if !ok {
		// A stage output is runtime-produced.
		return false
	}

	return k.returnKnown(prog, sub, r.Output, lenOnly)
}

// returnKnown resolves a ref THROUGH a plain sub-pipeline call: the ref's
// first output segment selects the return binding, whose value is examined in
// the callee's own scope. A deeper projection preserves known-ness — a
// projection of an invocation-known value is invocation-known, and a
// projection of a mapped-call output keeps the fork-count outer dimension. A
// whole-outs ref ("") requires every return binding known.
func (k invKnown) returnKnown(prog *ir.Program, sub *ir.Pipeline, output string, lenOnly bool) bool {
	if output == "" {
		if len(sub.Returns) == 0 {
			return false
		}

		for _, b := range sub.Returns {
			if !k.known(prog, sub, b.Value, lenOnly) {
				return false
			}
		}

		return true
	}

	field, _, _ := strings.Cut(output, ".")

	for _, b := range sub.Returns {
		if b.Param == field {
			return k.known(prog, sub, b.Value, lenOnly)
		}
	}

	return false
}

// bindingFor returns the call's explicit binding for a parameter.
func bindingFor(c ir.Call, param string) (ir.Binding, bool) {
	for _, b := range c.Bindings {
		if b.Param == param {
			return b, true
		}
	}

	return ir.Binding{}, false
}

// valueHasRef reports whether a value expression contains any ref leaf.
func valueHasRef(v ir.Value) bool {
	if v.Ref != nil {
		return true
	}

	if slices.ContainsFunc(v.Array, valueHasRef) {
		return true
	}

	for _, el := range v.Object {
		if valueHasRef(el) {
			return true
		}
	}

	return false
}

// entryScoped reports whether p is the entry pipeline — instantiated exactly
// once, so a self ref's value is unambiguous. Shared by foldDisableOff and
// invKnown.known so the two "invocation-known" scope tests cannot drift.
func entryScoped(prog *ir.Program, p *ir.Pipeline) bool {
	return prog.Entry != nil && prog.Entry.Callable == p.Name
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
