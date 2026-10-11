package emit

import (
	"slices"
	"strconv"
	"strings"

	"github.com/eunmann/mro2nf/internal/ir"
)

// LiftForkIndependent moves each call of a map-called sub-pipeline that reads
// nothing of the map call's forks out of the map call, into the caller, so it
// runs once instead of once per fork (#433). mrp forks a stage only over the
// map calls its resolved inputs and disable reference
// (syntax/resolve_stage.go:320-351 resolveForks) and runs a stage with no fork
// root once, as fork0 (core/node.go:428-439 buildForks), whatever the fork
// count at run time — none included. The lifted call's outputs reach the
// residual sub-pipeline as extra broadcast inputs (__h_<call>), so every
// stage's bindings resolve to the same values.
//
// mrp drops a map call over a statically empty or null source together with
// its children (resolve_pipeline.go:101-112) and disables them with the map
// call (resolve_stage.go:179-202), so a lifted call carries a gate (ir.Call
// Gate) that reads the map call's split bindings and disable flag: one task
// per lift, not per fork. The pass walks plain and mapped sub-pipeline calls
// alike, so it also lifts what a plain call's callee would run per fork. It
// replaces prog.Pipelines entries and never mutates an existing pipeline, so
// internal/overrides runs it on a shallow copy to name the lifted calls'
// processes as Emit does.
func LiftForkIndependent(prog *ir.Program) {
	if prog.Entry == nil {
		return
	}

	l := &lifter{prog: prog, done: map[string]bool{}, made: map[string]bool{}}
	l.process(prog.Entry.Callable)

	// A residual copy made for a lift that an enclosing lift then refused is
	// unreachable; only such copies go, never a declared pipeline.
	reachable := reachablePipelines(prog)
	for name := range l.made {
		if !reachable[name] {
			delete(prog.Pipelines, name)
		}
	}
}

// lifter carries the pass's program, the pipelines it has processed and the
// residual copies it made.
type lifter struct {
	prog *ir.Program
	done map[string]bool
	made map[string]bool
}

// process lifts, in pipeline name, every map call's fork-independent calls
// out of it, then processes each pipeline name calls.
func (l *lifter) process(name string) {
	p, ok := l.prog.Pipelines[name]
	if !ok || l.done[name] {
		return
	}

	l.done[name] = true

	np := clonePipeline(p, p.Name)
	changed := false

	for i := 0; i < len(np.Calls); i++ {
		c := np.Calls[i]
		if sub, ok := l.prog.Pipelines[c.Callable]; ok && c.Mapped {
			if n := l.liftOut(np, i, sub, splitParams(c), true); n > 0 {
				i += n
				changed = true
			}
		}
	}

	if changed {
		l.prog.Pipelines[name] = np
	}

	for _, c := range np.Calls {
		l.process(c.Callable)
	}
}

// liftOut lifts the calls of sub, the callee of s.Calls[i], that do not
// depend on sub's inputs in dep, into s before that call, retargets the call
// to the residual copy of sub, and returns how many calls it inserted. prune
// gates them on the call's split bindings too (gateLifted).
func (l *lifter) liftOut(s *ir.Pipeline, i int, sub *ir.Pipeline, dep map[string]bool, prune bool) int {
	x := s.Calls[i]

	inner, lifted, refd, ok := l.escape(sub, dep)
	if !ok {
		return 0
	}

	calls, dead, ok := rewriteLifted(x, sub, lifted, l.prog)
	if !ok {
		return 0
	}

	calls = gateLifted(x, calls, prune)
	if !namesFree(s, calls) {
		return 0
	}

	nx := x
	nx.Callable = inner.Name
	nx.Bindings = slices.Clone(x.Bindings)

	for _, id := range refd {
		v, isDead := dead[x.Name+"_"+id]
		if !isDead {
			v = ir.Value{Ref: &ir.Ref{Kind: ir.RefKindCall, ID: x.Name + "_" + id}}
		}

		nx.Bindings = append(nx.Bindings, ir.Binding{Param: liftedParam(id), Value: v})
	}

	l.prog.Pipelines[inner.Name] = inner
	s.Calls = slices.Concat(s.Calls[:i], calls, []ir.Call{nx}, s.Calls[i+1:])

	return len(calls)
}

// escape returns a copy of sub without its calls that depend on none of its
// inputs in dep, those calls (in sub's scope), and the ones the copy reads
// through a liftedParam input. It first lifts, into the copy, what its
// dependent sub-pipeline calls would run regardless of dep, so those calls
// leave too. ok is false when nothing is independent.
func (l *lifter) escape(sub *ir.Pipeline, dep map[string]bool) (*ir.Pipeline, []ir.Call, []string, bool) {
	s := clonePipeline(sub, l.freshName(sub.Name+"_lift"))

	dependent := callDependence(s, dep)

	for i := 0; i < len(s.Calls); i++ {
		x := s.Calls[i]

		callee, ok := l.prog.Pipelines[x.Callable]
		if !ok || !dependent[x.Name] || !disableIndependent(x, dep, dependent) {
			continue
		}

		// mrp forks a call over the map calls its inputs and disable read, not
		// over the source of a map call it sits in, so a call x's callee runs
		// regardless of dep leaves even when x's own splits depend on dep; it is
		// then not pruned with x, which only fork-dependent sources would do.
		prune := splitsIndependent(x, dep, dependent)
		if n := l.liftOut(s, i, callee, boundDependence(x, dep, dependent), prune); n > 0 {
			i += n
			dependent = callDependence(s, dep)
		}
	}

	var keep, lifted []ir.Call

	for _, c := range s.Calls {
		if dependent[c.Name] {
			keep = append(keep, c)
		} else {
			lifted = append(lifted, c)
		}
	}

	if len(lifted) == 0 {
		return nil, nil, nil, false
	}

	refd := residualRefs(s, keep, lifted, l.prog)

	return s, lifted, refd, true
}

// residualRefs makes s the residual of lifting: keep are its calls, each ref
// to a lifted call reads the liftedParam input it adds instead. It returns the
// lifted calls those refs name.
func residualRefs(s *ir.Pipeline, keep, lifted []ir.Call, prog *ir.Program) []string {
	moved := map[string]ir.Call{}
	for _, c := range lifted {
		moved[c.Name] = c
	}

	var refd []string

	redirect := func(r *ir.Ref) *ir.Ref {
		c, ok := moved[r.ID]
		if r.Kind != ir.RefKindCall || !ok {
			return r
		}

		if !slices.Contains(refd, r.ID) {
			refd = append(refd, r.ID)
			s.In = append(s.In, liftedInput(c, prog))
		}

		return &ir.Ref{Kind: ir.RefKindSelf, ID: liftedParam(r.ID), Output: r.Output}
	}

	for i := range keep {
		keep[i].Bindings = mapBindingRefs(keep[i].Bindings, redirect)
		if keep[i].Disabled != nil {
			keep[i].Disabled = redirect(keep[i].Disabled)
		}
	}

	s.Calls = keep
	s.Returns = mapBindingRefs(s.Returns, redirect)

	return refd
}

// rewriteLifted rewrites calls lifted out of sub, called by x, into the scope
// of s, the pipeline holding x: each is named <x>_<call>, a ref to an input of
// sub reads x's binding for it, and a wildcard binding, whose expansion the
// call also carries, is dropped. A gate that x binds to a literal folds as
// inlining folds it: false drops the gate, true drops the call, whose outputs
// (in the dead map returned) are null. ok is false when a value cannot
// compose, as inlining refuses it.
func rewriteLifted(x ir.Call, sub *ir.Pipeline, lifted []ir.Call, prog *ir.Program) ([]ir.Call, map[string]ir.Value, bool) {
	self := map[string]ir.Value{}

	for _, b := range x.Bindings {
		if (b.Value.Array != nil || b.Value.Object != nil) && paramHasMap(prog, sub, b.Param) {
			return nil, nil, false
		}

		self[b.Param] = b.Value
	}

	out := make([]ir.Call, 0, len(lifted))
	dead := map[string]ir.Value{}

	for _, c := range lifted {
		nc := c
		nc.Name = x.Name + "_" + c.Name

		bs, ok := rewriteBindings(slices.DeleteFunc(slices.Clone(c.Bindings), isWildcard), self, x.Name)
		if !ok {
			return nil, nil, false
		}

		nc.Bindings = bs

		if c.Disabled != nil {
			keep, ok := spliceDisable(&nc, self, x.Name)
			if !ok {
				return nil, nil, false
			}

			if !keep {
				dead[nc.Name] = nullOutput()

				continue
			}
		}

		out = append(out, nc)
	}

	return resolveDeadInternal(out, dead), dead, true
}

// gateLifted gates the calls lifted out of x as mrp disables x's children:
// a gate reads x's disable flag and, under prune for a map call whose splits
// are not all literals (which are never empty), its split bindings. A lifted
// call with its own disable gets a gate of both flags. Gate calls themselves
// stay ungated.
func gateLifted(x ir.Call, calls []ir.Call, prune bool) []ir.Call {
	var flags []ir.Binding

	if prune && x.Mapped && !splitsLiteral(x) {
		for _, b := range x.Bindings {
			if b.Split {
				flags = append(flags, b)
			}
		}
	}

	if x.Disabled != nil {
		flags = append(flags, gateFlag("@disabled", x.Disabled))
	}

	if len(flags) == 0 {
		return calls
	}

	group := ir.Call{Name: x.Name + "_gate", Gate: true, MapMode: x.MapMode, Bindings: flags}
	out := []ir.Call{group}

	for _, c := range calls {
		switch {
		case c.Gate:
		case c.Disabled == nil:
			c.Disabled = gateOff(group.Name)
		default:
			own := ir.Call{Name: c.Name + "_gate", Gate: true, Bindings: []ir.Binding{
				gateFlag("@group", gateOff(group.Name)), gateFlag("@disabled", c.Disabled),
			}}
			out = append(out, own)
			c.Disabled = gateOff(own.Name)
		}

		out = append(out, c)
	}

	return out
}

// namesFree reports whether the calls about to join s have distinct names
// that no call of s has.
func namesFree(s *ir.Pipeline, calls []ir.Call) bool {
	seen := map[string]bool{}

	for _, c := range s.Calls {
		seen[c.Name] = true
	}

	for _, c := range calls {
		if seen[c.Name] {
			return false
		}

		seen[c.Name] = true
	}

	return true
}

// gateFlag is a gate's non-split binding to the flag at r.
func gateFlag(param string, r *ir.Ref) ir.Binding {
	return ir.Binding{Param: param, Value: ir.Value{Ref: r}}
}

// gateOff is the ref to a gate's off output.
func gateOff(gate string) *ir.Ref {
	return &ir.Ref{Kind: ir.RefKindCall, ID: gate, Output: ir.GateOutput}
}

// splitsLiteral reports whether every split binding of c is a ref-free
// literal: Martian rejects an empty literal split, so such a map call is never
// pruned for its source.
func splitsLiteral(c ir.Call) bool {
	for _, b := range c.Bindings {
		if b.Split && valueHasRef(b.Value) {
			return false
		}
	}

	return true
}

// callDependence reports, per call of s, whether it reads (through a binding,
// its disable, or a call it reads) an input of s in dep. A wildcard binding
// is skipped: its expansion is among the call's bindings.
func callDependence(s *ir.Pipeline, dep map[string]bool) map[string]bool {
	out := map[string]bool{}
	visiting := map[string]bool{}

	var callDep func(name string) bool

	valueDep := func(v ir.Value) bool { return refDependent(v, dep, callDep) }

	callDep = func(name string) bool {
		if v, ok := out[name]; ok || visiting[name] {
			return v
		}

		visiting[name] = true

		c, _ := callByName(s, name)
		d := c.Disabled != nil && valueDep(ir.Value{Ref: c.Disabled})

		for _, b := range c.Bindings {
			d = d || (!isWildcard(b) && valueDep(b.Value))
		}

		out[name] = d

		return d
	}

	for _, c := range s.Calls {
		callDep(c.Name)
	}

	return out
}

// refDependent reports whether v holds a ref to an input in dep or to a call
// callDep reports dependent.
func refDependent(v ir.Value, dep map[string]bool, callDep func(string) bool) bool {
	if r := v.Ref; r != nil {
		if r.Kind == ir.RefKindSelf {
			return dep[r.ID]
		}

		return callDep(r.ID)
	}

	if slices.ContainsFunc(v.Array, func(e ir.Value) bool { return refDependent(e, dep, callDep) }) {
		return true
	}

	for _, e := range v.Object {
		if refDependent(e, dep, callDep) {
			return true
		}
	}

	return false
}

// disableIndependent reports whether x's disable depends on nothing in dep:
// mrp forks a call over its disable's map calls too, so only then can what
// x's callee runs regardless of x's inputs leave x with x's gate.
func disableIndependent(x ir.Call, dep, dependent map[string]bool) bool {
	callDep := func(name string) bool { return dependent[name] }

	return x.Disabled == nil || !refDependent(ir.Value{Ref: x.Disabled}, dep, callDep)
}

// splitsIndependent reports whether x's split bindings depend on nothing in
// dep, so a gate in x's scope can read them.
func splitsIndependent(x ir.Call, dep, dependent map[string]bool) bool {
	callDep := func(name string) bool { return dependent[name] }

	for _, b := range x.Bindings {
		if b.Split && refDependent(b.Value, dep, callDep) {
			return false
		}
	}

	return true
}

// boundDependence is the set of x's callee inputs that depend on dep: those x
// binds to a dependent value, and for a map call those it splits.
func boundDependence(x ir.Call, dep, dependent map[string]bool) map[string]bool {
	callDep := func(name string) bool { return dependent[name] }
	out := map[string]bool{}

	for _, b := range x.Bindings {
		if !isWildcard(b) && (b.Split || refDependent(b.Value, dep, callDep)) {
			out[b.Param] = true
		}
	}

	return out
}

// splitParams is the set of a map call's split params.
func splitParams(c ir.Call) map[string]bool {
	out := map[string]bool{}

	for _, b := range c.Bindings {
		if b.Split {
			out[b.Param] = true
		}
	}

	return out
}

// liftedParam names the residual input that carries lifted call id's outputs.
func liftedParam(id string) string {
	return "__h_" + id
}

// liftedInput is the residual input for lifted call c: its callable's struct,
// with the fork dimension of a map call.
func liftedInput(c ir.Call, prog *ir.Program) ir.Param {
	mapDim, arrayDim := forkDims(&c)
	if !c.Mapped {
		mapDim, arrayDim = 0, 0
	}

	typ := c.Callable
	var typSb480 strings.Builder
	for range arrayDim {
		typSb480.WriteString("[]")
	}
	typ += typSb480.String()

	if mapDim > 0 {
		typ = "map<" + typ + ">"
	}

	_, isStruct := prog.Structs[c.Callable]
	base := c.Callable

	if !isStruct {
		base = ""
	}

	return ir.Param{Name: liftedParam(c.Name), Type: typ, BaseType: base, ArrayDim: arrayDim, MapDim: mapDim}
}

// isWildcard reports a `* = self` binding.
func isWildcard(b ir.Binding) bool {
	return b.Param == "*"
}

// mapBindingRefs returns bs with every ref leaf replaced by fn's result.
func mapBindingRefs(bs []ir.Binding, fn func(*ir.Ref) *ir.Ref) []ir.Binding {
	out := make([]ir.Binding, len(bs))

	for i, b := range bs {
		out[i] = ir.Binding{Param: b.Param, Value: mapValueRefs(b.Value, fn), Split: b.Split}
	}

	return out
}

// mapValueRefs returns v with every ref leaf replaced by fn's result.
func mapValueRefs(v ir.Value, fn func(*ir.Ref) *ir.Ref) ir.Value {
	switch {
	case v.Ref != nil:
		return ir.Value{Ref: fn(v.Ref)}
	case v.Array != nil:
		arr := make([]ir.Value, len(v.Array))
		for i, e := range v.Array {
			arr[i] = mapValueRefs(e, fn)
		}

		return ir.Value{Array: arr}
	case v.Object != nil:
		obj := make(map[string]ir.Value, len(v.Object))
		for k, e := range v.Object {
			obj[k] = mapValueRefs(e, fn)
		}

		return ir.Value{Object: obj}
	default:
		return v
	}
}

// clonePipeline copies p under name with its own In, Calls and Returns.
func clonePipeline(p *ir.Pipeline, name string) *ir.Pipeline {
	np := *p
	np.Name = name
	np.In = slices.Clone(p.In)
	np.Calls = slices.Clone(p.Calls)
	np.Returns = slices.Clone(p.Returns)

	return &np
}

// freshName returns base<n>, the first that names no pipeline, stage or
// struct of the program and no copy the pass made.
func (l *lifter) freshName(base string) string {
	for n := 1; ; n++ {
		cand := base + strconv.Itoa(n)

		_, pipe := l.prog.Pipelines[cand]
		_, stage := l.prog.Stages[cand]
		_, strct := l.prog.Structs[cand]

		if !pipe && !stage && !strct && !l.made[cand] {
			l.made[cand] = true

			return cand
		}
	}
}
