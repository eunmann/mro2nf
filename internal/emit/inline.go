package emit

import (
	"strings"

	"github.com/eunmann/mro2nf/internal/ir"
)

// inlinePipelines flattens eligible sub-pipeline calls into their parent, so the
// pipeline boundary — and the entry/return BIND tasks the emitter materializes
// for it — disappears (#221). A Martian pipeline has no runtime identity (mrp
// emits no pipeline jobs), so its boundary bind is pure orchestration overhead.
// This changes ONLY orchestration: every leaf stage, its bindings' resolved
// values, and the data plane are untouched (the north-star contract), so outputs
// stay byte-identical — a leaf stage that read self.x.y now reads the parent's
// binding for x projected by .y, the exact same value.
//
// Conservative by construction: a call is inlined only when its callee is an
// unmapped sub-pipeline AND every self-input substitution and every consumer
// projection composes cleanly (a projection is only ever pushed onto a
// ref/object/array, never into a scalar literal). A runtime-disabled call is
// inlined only when its gate can be pushed onto every internal call as a single
// ref while staying byte-identical AND without increasing task count — the
// boundary bind of a disabled sub cheaply short-circuits the whole sub-workflow,
// so inlining is admitted only for a flat fan-out sub whose promoted calls the
// emitter fuses into gated binds (inlineDisabled). Anything else is left as a
// boundary task, so the pass can only remove tasks, never alter output.
func inlinePipelines(prog *ir.Program) {
	before := reachablePipelines(prog)

	flat := map[string]bool{}
	for name := range prog.Pipelines {
		flattenPipeline(prog.Pipelines[name], prog, flat)
	}

	// Prune only the sub-pipelines inlining just made unreachable — those whose
	// every call site was flattened into a parent. Their modules are now dead
	// code the emitter would otherwise still emit as uninvoked sub-workflows (no
	// runtime tasks, but clutter in the DAG and the emitted process set). A
	// pipeline already unreachable BEFORE inlining (e.g. a #59 dead map) is left
	// untouched, keeping this scoped to cleaning up inline's own dead modules.
	after := reachablePipelines(prog)
	for name := range prog.Pipelines {
		if before[name] && !after[name] {
			delete(prog.Pipelines, name)
		}
	}
}

// reachablePipelines is the set of pipeline names reachable from the entry via
// the call graph (following every call — plain, mapped, or disabled). A stage
// callable is not a pipeline and simply terminates that branch.
func reachablePipelines(prog *ir.Program) map[string]bool {
	reachable := map[string]bool{}
	if prog.Entry == nil {
		return reachable
	}

	var visit func(string)
	visit = func(name string) {
		p, ok := prog.Pipelines[name]
		if !ok || reachable[name] {
			return // a stage callable, or already visited
		}

		reachable[name] = true
		for _, c := range p.Calls {
			visit(c.Callable)
		}
	}

	visit(prog.Entry.Callable)

	return reachable
}

// flattenPipeline inlines every eligible sub-pipeline call in p (recursing into
// each callee first, so a nested sub-pipeline is already flat when spliced),
// memoized by name so a shared pipeline is flattened once. It runs one joint
// fixpoint of three monotonic steps — inline, resolve inlined-output refs, fold
// constant disables — each of which only removes work, so it converges.
func flattenPipeline(p *ir.Pipeline, prog *ir.Program, flat map[string]bool) {
	if flat[p.Name] {
		return
	}

	flat[p.Name] = true

	subs := map[string]ir.Value{} // inlined call name -> its whole output (an Object of the sub's returns)
	for flattenRound(p, prog, flat, subs) {
	}
}

// flattenRound performs one inline → resolve → fold pass, reporting whether it
// changed anything (so flattenPipeline can iterate to a fixpoint).
func flattenRound(p *ir.Pipeline, prog *ir.Program, flat map[string]bool, subs map[string]ir.Value) bool {
	changed := inlineEligible(p, prog, flat, subs)
	changed = resolveOutputRefs(p, subs) || changed

	return foldDisables(p, subs) || changed
}

// inlineEligible splices every eligible plain sub-pipeline call into p, recording
// each inlined call's whole output in subs. Both undisabled and safely
// runtime-disabled calls are eligible (see inlineOne).
func inlineEligible(p *ir.Pipeline, prog *ir.Program, flat map[string]bool, subs map[string]ir.Value) bool {
	changed := false

	out := p.Calls[:0:0]
	for _, c := range p.Calls {
		sub, ok := prog.Pipelines[c.Callable]
		if !ok || c.Mapped {
			out = append(out, c)

			continue
		}

		flattenPipeline(sub, prog, flat)

		spliced, whole, ok := inlineOne(c, sub, prog, subs)
		if !ok {
			out = append(out, c) // un-composable or unsafe: keep the boundary task

			continue
		}

		out = append(out, spliced...)
		subs[c.Name] = whole
		changed = true
	}

	p.Calls = out

	return changed
}

// inlineOne inlines a plain sub-pipeline call, dispatching on its disable gate:
// an undisabled call inlines directly; a runtime-disabled call inlines only when
// the gate can be pushed onto every spliced internal call both safely and without
// increasing orchestration (inlineDisabled). A gate that folds to a constant is
// left to foldDisables (ok=false, boundary kept, then re-visited next round
// undisabled or pruned).
func inlineOne(c ir.Call, sub *ir.Pipeline, prog *ir.Program, subs map[string]ir.Value) ([]ir.Call, ir.Value, bool) {
	if c.Disabled == nil {
		return inlineCall(c, sub)
	}

	return inlineDisabled(c, sub, prog, subs)
}

// inlineDisabled inlines a plain sub-pipeline call gated by a runtime disable X,
// pushing X onto every spliced internal call so the whole sub is skipped — and
// its call-output-ref returns resolve to null — exactly when X is true, which is
// byte-identical to mrp disabling the sub-pipeline. It inlines only when BOTH gates
// pass: disabledInlineSafe (correctness — the gate stays a single ref and every
// return nulls) and disabledInlineProfitable (overhead — the promoted calls fuse
// into gated binds, so a disabled fork never costs more tasks than the boundary).
// A gate that folds to a constant is deferred to foldDisables. Any failure →
// ok=false, boundary kept.
func inlineDisabled(c ir.Call, sub *ir.Pipeline, prog *ir.Program, subs map[string]ir.Value) ([]ir.Call, ir.Value, bool) {
	gate, runtime := runtimeGate(c.Disabled, subs)
	if !runtime || !disabledInlineSafe(sub) || !disabledInlineProfitable(gate, sub, prog) {
		return nil, ir.Value{}, false
	}

	spliced, whole, ok := inlineCall(c, sub)
	if !ok {
		return nil, ir.Value{}, false
	}

	for i := range spliced {
		g := *gate
		spliced[i].Disabled = &g // every internal call inherits X (none had its own)
	}

	return spliced, whole, true
}

// runtimeGate resolves a disable gate through the inlined-output subs, reporting
// whether it is a genuine runtime ref (true) — an unresolved self/upstream ref, or
// one that resolves to another ref — versus a gate that folds to a constant
// (false), which foldDisables handles instead.
func runtimeGate(d *ir.Ref, subs map[string]ir.Value) (*ir.Ref, bool) {
	nv, resolved := applySubToValue(ir.Value{Ref: d}, subs)
	if !resolved {
		return d, true
	}

	return nv.Ref, nv.Ref != nil
}

// disabledInlineSafe reports whether sub can be inlined under a runtime disable:
// no internal call may carry its own disable (the gate must stay a single ref),
// and every return leaf must be a call-output ref (so it nulls with its now-
// disabled call, matching mrp nulling every output of a disabled sub-pipeline).
func disabledInlineSafe(sub *ir.Pipeline) bool {
	for _, ic := range sub.Calls {
		if ic.Disabled != nil {
			return false
		}
	}

	for _, rb := range sub.Returns {
		if !isCallOutputRef(rb.Value) {
			return false
		}
	}

	return true
}

// disabledInlineProfitable reports whether inlining sub under gate X cannot
// INCREASE orchestration tasks. A disabled sub's boundary is a single cheap bind
// that short-circuits the entire sub-workflow when X fires; inlining promotes the
// sub's internal calls into the parent, and the emitter runs a STANDALONE,
// UNCONDITIONAL bind for any promoted call it cannot fuse (a mapped, split,
// preflight, non-native-gated, or internally-chained call). Such a call would pay
// its bind even while disabled, so a runtime-disabled sub could cost M binds
// instead of 1. We therefore inline only a FLAT fan-out sub: X is natively
// readable and every internal call is an unmapped, non-preflight, non-split leaf
// stage reading only pipeargs (no internal chaining) — exactly the calls the
// emitter fuses into a gated bind, so a disabled fork costs ZERO tasks and an
// enabled one drops the boundary bind. A strict non-regression either way.
func disabledInlineProfitable(gate *ir.Ref, sub *ir.Pipeline, prog *ir.Program) bool {
	if _, _, ok := nativeDisableGate(ir.Call{Disabled: gate}); !ok {
		return false
	}

	for _, ic := range sub.Calls {
		if ic.Mapped || ic.Preflight {
			return false
		}

		if s, ok := prog.Stages[ic.Callable]; !ok || s.Split {
			return false
		}

		if bindingsRefCall(ic.Bindings) {
			return false // an internally-chained call: its promoted bind is not free
		}
	}

	return true
}

// isCallOutputRef reports whether v is a bare reference to a call's output — not a
// literal, a self/pipeline-input ref, or a composite array/object (each of which
// would fail to null when the sub is disabled at runtime).
func isCallOutputRef(v ir.Value) bool {
	return v.Ref != nil && v.Ref.Kind == ir.RefKindCall
}

// resolveOutputRefs rewrites every ref to an inlined call's output (in p's calls
// and returns) to the sub-pipeline's rewritten return value.
func resolveOutputRefs(p *ir.Pipeline, subs map[string]ir.Value) bool {
	changed := false

	for i := range p.Calls {
		var c bool
		p.Calls[i].Bindings, c = applySubs(p.Calls[i].Bindings, subs)
		changed = changed || c
	}

	var rc bool
	p.Returns, rc = applySubs(p.Returns, subs)

	return changed || rc
}

// foldDisables folds any disable gate inlining turned into a constant across p's
// calls — a literal false drops the gate; a literal true prunes the call to a
// null output (recorded in subs so its refs resolve to null next round).
func foldDisables(p *ir.Pipeline, subs map[string]ir.Value) bool {
	kept, changed := foldPrune(p.Calls, subs)
	p.Calls = kept

	return changed
}

// foldPrune folds every constant disable across calls against the resolved-output
// subs: a constant-true gate drops the call and records its null output in subs;
// a constant-false gate is cleared. It returns the surviving calls and whether
// anything changed. Shared by the outer flatten fixpoint (foldDisables) and the
// inner dead-call fixpoint (resolveDeadInternal).
func foldPrune(calls []ir.Call, subs map[string]ir.Value) ([]ir.Call, bool) {
	changed := false

	kept := calls[:0:0]
	for _, c := range calls {
		keep, dead, c2 := foldDisable(&c, subs)
		changed = changed || c2

		switch {
		case dead:
			subs[c.Name] = nullOutput()
		case keep:
			kept = append(kept, c)
		}
	}

	return kept, changed
}

// nullOutput is an inlined/pruned call's whole output when all of it is null: an
// empty Object, so every projection into it resolves to null (composeProjection).
func nullOutput() ir.Value {
	return ir.Value{Object: map[string]ir.Value{}}
}

// foldDisable resolves a call's disable gate through the inlined-output subs,
// returning (keep, dead, changed): a gate that resolves to a ref is rewritten; a
// literal false/null drops the gate; a literal true prunes the call (dead).
func foldDisable(c *ir.Call, subs map[string]ir.Value) (bool, bool, bool) {
	if c.Disabled == nil {
		return true, false, false
	}

	nv, resolved := applySubToValue(ir.Value{Ref: c.Disabled}, subs)
	if !resolved {
		return true, false, false // gate ref not into an inlined output (self/upstream) — unchanged
	}

	if nv.Ref != nil {
		c.Disabled = nv.Ref

		return true, false, true
	}

	if litIsTrue(nv) {
		return false, true, true // constant-true gate: prune to null
	}

	if litIsFalse(nv) {
		c.Disabled = nil // constant false/null gate: the call always runs

		return true, false, true
	}

	// Unrecognized gate value (e.g. a composite — a type error in valid MRO):
	// fail CLOSED. Leave the gate untouched rather than clearing it, which would
	// wrongly enable the call.
	return true, false, false
}

// litIsTrue reports whether v is the boolean literal true.
func litIsTrue(v ir.Value) bool {
	return strings.TrimSpace(string(v.Literal)) == "true"
}

// litIsFalse reports whether v is a constant-false gate: the boolean literal
// false or JSON null (a null disable gate never fires, so the call always runs).
func litIsFalse(v ir.Value) bool {
	return strings.TrimSpace(string(v.Literal)) == "false" || isNullLiteral(v)
}

// inlineCall builds the renamed, ref-rewritten calls that replace call c to
// sub-pipeline sub, plus c's whole output as an Object of the sub's (rewritten)
// return values — so a ref to c or any of its outputs resolves by projecting
// into that Object. Returns ok=false if any self-substitution or return would
// push a projection into a scalar literal (not expressible as a ref path).
func inlineCall(c ir.Call, sub *ir.Pipeline) ([]ir.Call, ir.Value, bool) {
	self := map[string]ir.Value{} // sub input name -> parent's bound value
	for _, b := range c.Bindings {
		self[b.Param] = b.Value
	}

	spliced, dead, ok := spliceCalls(c.Name, sub.Calls, self)
	if !ok {
		return nil, ir.Value{}, false
	}

	whole := map[string]ir.Value{}
	for _, rb := range sub.Returns {
		rv, ok := rewriteValue(rb.Value, self, c.Name)
		if !ok {
			return nil, ir.Value{}, false
		}

		whole[rb.Param] = rv
	}

	// Resolve any ref to a pruned-dead internal call to null across the spliced
	// calls and the whole-output, to a fixpoint (a dead call's null may fold a
	// further disable) — the same monotonic convergence as the outer flatten.
	if len(dead) > 0 {
		spliced = resolveDeadInternal(spliced, dead)
		for k, v := range whole {
			whole[k], _ = applySubToValue(v, dead)
		}
	}

	return spliced, ir.Value{Object: whole}, true
}

// spliceCalls renames and rewrites each of a sub-pipeline's internal calls into
// the parent scope. It folds a self-substituted constant disable inline (false
// drops the gate; true records the call in dead and drops it), and returns
// ok=false if a binding cannot compose.
func spliceCalls(prefix string, calls []ir.Call, self map[string]ir.Value) ([]ir.Call, map[string]ir.Value, bool) {
	dead := map[string]ir.Value{}
	spliced := make([]ir.Call, 0, len(calls))

	for _, ic := range calls {
		nic := ic
		nic.Name = prefix + "_" + ic.Name

		bs, ok := rewriteBindings(ic.Bindings, self, prefix)
		if !ok {
			return nil, nil, false
		}

		nic.Bindings = bs

		if ic.Disabled != nil {
			keep, ok := spliceDisable(&nic, self, prefix)
			if !ok {
				return nil, nil, false
			}

			if !keep {
				dead[nic.Name] = nullOutput()

				continue
			}
		}

		spliced = append(spliced, nic)
	}

	return spliced, dead, true
}

// spliceDisable rewrites an inlined call's disable into the parent scope: a ref
// stays a gate, a literal false/null drops it (keep, gate cleared), a literal
// true prunes the call (keep=false). ok=false if the disable cannot compose.
func spliceDisable(nic *ir.Call, self map[string]ir.Value, prefix string) (bool, bool) {
	dv, ok := rewriteValue(ir.Value{Ref: nic.Disabled}, self, prefix)
	if !ok {
		return false, false
	}

	switch {
	case dv.Ref != nil:
		nic.Disabled = dv.Ref
	case litIsTrue(dv):
		return false, true // pruned
	case litIsFalse(dv):
		nic.Disabled = nil // literal false/null gate: the call always runs
	default:
		// Unrecognized gate value (a type error in valid MRO): fail CLOSED —
		// abort the inline and keep the boundary rather than enabling the call.
		return false, false
	}

	return true, true
}

// resolveDeadInternal rewrites refs to pruned internal calls (dead) to null and
// folds any disable that becomes constant, pruning further dead calls, to a
// fixpoint over the spliced set.
func resolveDeadInternal(calls []ir.Call, dead map[string]ir.Value) []ir.Call {
	for {
		changed := false
		for i := range calls {
			var c bool
			calls[i].Bindings, c = applySubs(calls[i].Bindings, dead)
			changed = changed || c
		}

		var pruned bool
		calls, pruned = foldPrune(calls, dead)

		if !changed && !pruned {
			return calls
		}
	}
}

// rewriteBindings rewrites a spliced call's bindings from the sub-pipeline's
// scope into the parent's: self.X -> the parent's binding for X (projection
// composed), call.IC -> the prefixed inlined name.
func rewriteBindings(bs []ir.Binding, self map[string]ir.Value, prefix string) ([]ir.Binding, bool) {
	out := make([]ir.Binding, len(bs))
	for i, b := range bs {
		v, ok := rewriteValue(b.Value, self, prefix)
		if !ok {
			return nil, false
		}

		out[i] = ir.Binding{Param: b.Param, Value: v, Split: b.Split}
	}

	return out, true
}

// rewriteValue rewrites a value from a sub-pipeline's scope into its parent's.
func rewriteValue(v ir.Value, self map[string]ir.Value, prefix string) (ir.Value, bool) {
	switch {
	case v.Literal != nil:
		return v, true
	case v.Array != nil:
		arr := make([]ir.Value, len(v.Array))
		for i, e := range v.Array {
			ev, ok := rewriteValue(e, self, prefix)
			if !ok {
				return ir.Value{}, false
			}

			arr[i] = ev
		}

		return ir.Value{Array: arr}, true
	case v.Object != nil:
		obj := make(map[string]ir.Value, len(v.Object))
		for k, e := range v.Object {
			ev, ok := rewriteValue(e, self, prefix)
			if !ok {
				return ir.Value{}, false
			}

			obj[k] = ev
		}

		return ir.Value{Object: obj}, true
	case v.Ref != nil:
		return rewriteRef(*v.Ref, self, prefix)
	}

	return v, true // empty value (null)
}

// rewriteRef rewrites a single ref. A self ref becomes the parent's bound value
// with the ref's projection path composed onto it; a call ref is re-prefixed to
// its inlined name.
func rewriteRef(r ir.Ref, self map[string]ir.Value, prefix string) (ir.Value, bool) {
	if r.Kind == ir.RefKindCall {
		return ir.Value{Ref: &ir.Ref{Kind: ir.RefKindCall, ID: prefix + "_" + r.ID, Output: r.Output}}, true
	}

	base, ok := self[r.ID]
	if !ok {
		return ir.Value{}, false // an unbound self input (should not happen post-lower)
	}

	return composeProjection(base, r.Output)
}

// composeProjection applies projection path onto base, resolving it fully: a ref
// extends its Output; an Object picks the head field and recurses (a missing
// field is null, matching Martian); an Array projects the path over each element
// (Martian projects field access through arrays); a whole-field (empty path) is
// base unchanged. Projecting into a scalar literal is not expressible as a ref
// path, so it is left un-composable (that call keeps its boundary task).
func composeProjection(base ir.Value, path string) (ir.Value, bool) {
	switch {
	case path == "":
		return base, true
	case base.Ref != nil:
		return ir.Value{Ref: &ir.Ref{Kind: base.Ref.Kind, ID: base.Ref.ID, Output: joinOutput(base.Ref.Output, path)}}, true
	case base.Object != nil:
		key, rest, _ := strings.Cut(path, ".")
		field, ok := base.Object[key]
		if !ok {
			return ir.Value{Literal: []byte("null")}, true
		}

		return composeProjection(field, rest)
	case base.Array != nil:
		arr := make([]ir.Value, len(base.Array))
		for i, e := range base.Array {
			ev, ok := composeProjection(e, path)
			if !ok {
				return ir.Value{}, false
			}

			arr[i] = ev
		}

		return ir.Value{Array: arr}, true
	case isNullLiteral(base):
		// Martian projects any path into null as null (a disabled/absent upstream);
		// so a whole-null boundary output projected further stays null.
		return ir.Value{Literal: []byte("null")}, true
	default:
		return ir.Value{}, false // projecting into a non-null scalar literal (a type error in valid MRO)
	}
}

// isNullLiteral reports whether v is the JSON null literal.
func isNullLiteral(v ir.Value) bool {
	return v.Literal != nil && strings.TrimSpace(string(v.Literal)) == "null"
}

// joinOutput joins two projection path segments with a dot, dropping an empty
// side (a whole-field base + path is just path).
func joinOutput(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + "." + b
	}
}

// applySubs rewrites every ref to an inlined call (subs[id]) to its resolved
// value across a binding list, reporting whether anything changed.
func applySubs(bs []ir.Binding, subs map[string]ir.Value) ([]ir.Binding, bool) {
	changed := false

	out := make([]ir.Binding, len(bs))
	for i, b := range bs {
		v, c := applySubToValue(b.Value, subs)
		out[i] = ir.Binding{Param: b.Param, Value: v, Split: b.Split}
		changed = changed || c
	}

	return out, changed
}

// applySubToValue resolves refs to inlined-call outputs within one value by
// projecting the ref's path into the inlined call's whole output.
func applySubToValue(v ir.Value, subs map[string]ir.Value) (ir.Value, bool) {
	switch {
	case v.Array != nil:
		arr := make([]ir.Value, len(v.Array))
		hit := false
		for i, e := range v.Array {
			ev, c := applySubToValue(e, subs)
			arr[i] = ev
			hit = hit || c
		}

		return ir.Value{Array: arr}, hit
	case v.Object != nil:
		obj := make(map[string]ir.Value, len(v.Object))
		hit := false
		for k, e := range v.Object {
			ev, c := applySubToValue(e, subs)
			obj[k] = ev
			hit = hit || c
		}

		return ir.Value{Object: obj}, hit
	case v.Ref != nil && v.Ref.Kind == ir.RefKindCall:
		whole, ok := subs[v.Ref.ID]
		if !ok {
			return v, false
		}

		nv, ok := composeProjection(whole, v.Ref.Output)
		if !ok {
			return v, false // leave unresolved rather than corrupt (should not occur; returns are refs)
		}

		return nv, true
	}

	return v, false
}
