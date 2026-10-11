package emit

import (
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/eunmann/mro2nf/internal/ir"
)

// invKnown is the invocation-known analysis over the IR binding graph (#127):
// which mapped calls the LAUNCH INVOCATION alone sizes. Martian's resolver
// treats an invocation-known empty fork collection differently from a runtime
// one — observed against mrp 4.0.15: a static []/{} split source resolves the
// whole mapped call to NULL (the zero-fork call is pruned, and KnownLength
// propagation cascades the prune through value chains), while an
// upstream-produced empty or null collection merges to the typed empty ([] /
// {}). mro2nf keeps entry inputs overridable at launch, so the distinction is
// never baked statically; the analysis only ever WIDENS the emptyNull
// marking, and the marking is purely a runtime shape rule: the call's
// ZERO-fork MERGE emits null (bind.Merge emptyNull). Override the entry side
// to non-empty and the forks run and merge normally; override to empty and
// the result is null — exactly what mrp produces for that invocation.
type invKnown struct {
	// emptySplit marks, per (pipeline, map call), that some split binding's
	// emptiness is invocation-determined in EVERY instance of the pipeline, so
	// a zero-fork run implies mrp's resolver pruned the call to null. After
	// SplitKnownContexts every instance of a pipeline agrees, so the AND is
	// exact; on an unsplit program it is the conservative merge.
	emptySplit map[string]map[string]bool
}

// knownInvocation evaluates every pipeline instance reachable from the entry
// (knownEval) and merges each pipeline's per-instance marks into one plan.
func knownInvocation(prog *ir.Program) invKnown {
	k := invKnown{emptySplit: make(map[string]map[string]bool, len(prog.Pipelines))}

	for _, in := range evalKnown(prog).insts {
		name := in.p.Name

		marks, seen := k.emptySplit[name]
		if !seen {
			k.emptySplit[name] = maps.Clone(in.split)

			continue
		}

		for c, v := range marks {
			marks[c] = v && in.split[c]
		}
	}

	return k
}

// SplitKnownContexts gives every distinct invocation-known context of a
// sub-pipeline its own copy, so the empty-fork rule is decided per call site
// (#241): `call SUB as S1(xs = self.empty)` and `call SUB as S2(xs = GEN.xs)`
// need SUB's zero-fork merge to give null under S1 (mrp prunes the
// invocation-known empty) and the typed empty under S2. The first context of
// a pipeline keeps its name; each further one is a copy named <name>_ctx<n>
// whose callers are retargeted to it. A program whose call sites all agree is
// left untouched. It replaces prog.Pipelines entries with new values and never
// mutates an existing *ir.Pipeline, so a shallow copy of prog (with its own
// Pipelines map) leaves the original intact. internal/overrides runs it on the
// program it resolves so its per-call selectors name the copies too.
func SplitKnownContexts(prog *ir.Program) {
	e := evalKnown(prog)

	names := e.classNames()
	if names == nil {
		return
	}

	built := map[string]*ir.Pipeline{}

	for _, in := range e.insts {
		name := names[in.p.Name][in.id]
		if _, ok := built[name]; ok {
			continue
		}

		np := *in.p
		np.Name = name
		np.Calls = slices.Clone(in.p.Calls)

		for i := range np.Calls {
			if sub := in.subs[np.Calls[i].Name]; sub != nil {
				np.Calls[i].Callable = names[sub.p.Name][sub.id]
			}
		}

		built[name] = &np
	}

	maps.Copy(prog.Pipelines, built)
}

// knownFacts is what the launch invocation determines about one pipeline
// instance's inputs: val marks inputs whose VALUE is invocation-known; len
// additionally admits mapped-call cascade sources, whose LENGTH (not values)
// is invocation-determined — emptiness is a length fact, so len is what split
// marking consults.
type knownFacts struct {
	val map[string]bool
	len map[string]bool
}

// sig is the facts' canonical form over p's inputs, the instance memo key.
func (f knownFacts) sig(p *ir.Pipeline) string {
	var b strings.Builder

	for i := range p.In {
		n := p.In[i].Name
		b.WriteString(n + "=" + strconv.FormatBool(f.val[n]) + "/" + strconv.FormatBool(f.len[n]) + ",")
	}

	return b.String()
}

// knownInst is one instantiation context of a pipeline, evaluated: its input
// facts, the split marks of its map calls, and the callee instance of each
// sub-pipeline call. Instances with equal id plan identically (same marks,
// same callee classes), so they can share one emitted copy.
type knownInst struct {
	p     *ir.Pipeline
	facts knownFacts
	split map[string]bool
	subs  map[string]*knownInst
	seen  map[string]bool
	id    int
}

// knownEval evaluates pipeline instances top-down from the entry, memoized by
// (pipeline, facts). Each call is evaluated on demand, after the calls its
// bindings reference, so a callee's facts are final when it is instantiated
// and every memoized instance is reachable. Recursion follows the pipeline
// call DAG (Martian forbids recursive pipelines); a malformed cycle reads the
// partial state and terminates.
type knownEval struct {
	prog  *ir.Program
	memo  map[string]*knownInst
	insts []*knownInst
	ids   map[string]map[string]int
	taken map[string]bool
}

// evalKnown evaluates every instance reachable from an entry pipeline, whose
// inputs come only from the launch invocation (overrides included), so every
// one of them is invocation-known. A stage entry has no pipeline instances.
func evalKnown(prog *ir.Program) *knownEval {
	e := &knownEval{prog: prog, memo: map[string]*knownInst{}, ids: map[string]map[string]int{}}
	if prog.Entry == nil {
		return e
	}

	entry, ok := prog.Pipelines[prog.Entry.Callable]
	if !ok {
		return e
	}

	all := map[string]bool{}
	for i := range entry.In {
		all[entry.In[i].Name] = true
	}

	e.instance(entry, knownFacts{val: all, len: all})

	return e
}

// instance returns the evaluated instance of p under facts.
func (e *knownEval) instance(p *ir.Pipeline, facts knownFacts) *knownInst {
	key := p.Name + "\x00" + facts.sig(p)
	if in, ok := e.memo[key]; ok {
		return in
	}

	in := &knownInst{
		p: p, facts: facts, split: map[string]bool{}, subs: map[string]*knownInst{}, seen: map[string]bool{},
	}
	e.memo[key] = in

	for _, c := range p.Calls {
		e.ensure(in, c)
	}

	in.id = e.classID(in)
	e.insts = append(e.insts, in)

	return in
}

// classID numbers in's emission class within its pipeline in first-seen
// order: the split marks plus each sub-pipeline call's callee class.
func (e *knownEval) classID(in *knownInst) int {
	var b strings.Builder

	for _, c := range in.p.Calls {
		if c.Mapped {
			b.WriteString(c.Name + "=" + strconv.FormatBool(in.split[c.Name]) + ";")
		}

		if sub := in.subs[c.Name]; sub != nil {
			b.WriteString(c.Name + ">" + strconv.Itoa(sub.id) + ";")
		}
	}

	ids := e.ids[in.p.Name]
	if ids == nil {
		ids = map[string]int{}
		e.ids[in.p.Name] = ids
	}

	id, ok := ids[b.String()]
	if !ok {
		id = len(ids)
		ids[b.String()] = id
	}

	return id
}

// ensure evaluates call c of instance in once: a sub-pipeline callee's
// instance under the facts c's bindings give it, and a map call's split mark.
func (e *knownEval) ensure(in *knownInst, c ir.Call) {
	if in.seen[c.Name] {
		return
	}

	in.seen[c.Name] = true

	if q, ok := e.prog.Pipelines[c.Callable]; ok {
		in.subs[c.Name] = e.instance(q, e.calleeFacts(in, c, q))
	}

	if c.Mapped {
		in.split[c.Name] = e.splitKnown(in, c)
	}
}

// calleeFacts is what call c passes callee q. A split-bound input receives
// ELEMENTS of the caller's collection, so only a fully value-known collection
// qualifies even for len: a cascade source has invocation-known length but
// runtime element values. An input c leaves unbound (or binds via a wildcard)
// stays unknown.
func (e *knownEval) calleeFacts(in *knownInst, c ir.Call, q *ir.Pipeline) knownFacts {
	f := knownFacts{val: map[string]bool{}, len: map[string]bool{}}

	for i := range q.In {
		n := q.In[i].Name

		b, ok := bindingFor(c, n)
		if !ok {
			continue
		}

		f.val[n] = e.known(in, b.Value, false)
		f.len[n] = e.known(in, b.Value, !b.Split)
	}

	return f
}

// splitKnown is the per-call marking rule: ANY split binding whose emptiness
// is invocation-known flags the map call. ANY (not ALL) is sound for a MIXED
// zip: zipped splits must agree in length (bind fails loudly with errSplitLen
// on a mismatch), so a ZERO-fork run implies every side — the known side
// included — resolved empty, which is exactly the invocation whose statically
// visible 0-length makes mrp prune the call to null.
func (e *knownEval) splitKnown(in *knownInst, c ir.Call) bool {
	for _, b := range c.Bindings {
		if b.Split && e.known(in, b.Value, true) {
			return true
		}
	}

	return false
}

// known reports whether a binding value in instance in is invocation-known.
// With lenOnly=false the VALUE must be: a ref-free literal (an empty literal
// split is a Martian parse error, so literals only ever plumb non-split
// bindings), a self ref on a value-known input (every entry input is), or a
// plain sub-pipeline ref whose return chain is known. lenOnly=true relaxes
// value to LENGTH/emptiness knowledge, additionally admitting a mapped call's
// output: the merged collection's size equals the fork count, which is
// invocation-determined exactly when that call's own split source is (mrp's
// KnownLength cascade) — its zero-fork merge yields null, and any projection
// of null stays null. The whole-outs ref of a mapped call qualifies too (#435):
// the binder regroups it into one struct per fork, so its length is the fork
// count — unless the callee has no outputs, whose regroup is always empty. A
// composite CONTAINING
// refs (fan-in `[A.out, B.out]`) is rejected even though its static length
// makes the zero-fork case unreachable today — the guard is structural, not
// an unstated invariant. A disable gate does not affect known-ness (#383):
// mrp folds a literal gate statically (false runs the call, true makes its
// outputs null, which a consumer's zero-fork merge also gives), and under a
// runtime gate it either prunes the producer's known-empty split to null or
// rejects the consumer's split of the gated value as a binding error.
func (e *knownEval) known(in *knownInst, v ir.Value, lenOnly bool) bool {
	r := v.Ref
	if r == nil {
		return !valueHasRef(v)
	}

	if r.Kind == ir.RefKindSelf {
		// A PROJECTED self ref (r.Output != "") navigates INTO the input, and
		// len's length-only sources (cascade collections) carry runtime
		// element VALUES — length knowledge of the container proves nothing
		// about a projected field's length. So a projection always requires
		// VALUE knowledge; only a whole-field ref may lean on len. The guard
		// is structural, not a per-type argument about which projections
		// happen to preserve the outer length.
		if lenOnly && r.Output == "" {
			return in.facts.len[r.ID]
		}

		return in.facts.val[r.ID]
	}

	c, ok := callByName(in.p, r.ID)
	if !ok {
		return false
	}

	e.ensure(in, c)

	if c.Mapped {
		return lenOnly && (r.Output != "" || e.hasOutputs(c.Callable)) && in.split[c.Name]
	}

	sub := in.subs[c.Name]
	if sub == nil {
		// A stage output is runtime-produced.
		return false
	}

	return e.returnKnown(sub, r.Output, lenOnly)
}

// hasOutputs reports whether the named stage or pipeline declares an output.
func (e *knownEval) hasOutputs(callable string) bool {
	if s, ok := e.prog.Stages[callable]; ok {
		return len(s.Out) > 0
	}

	if p, ok := e.prog.Pipelines[callable]; ok {
		return len(p.Out) > 0
	}

	return false
}

// returnKnown resolves a ref THROUGH a plain sub-pipeline call: the ref's
// first output segment selects the return binding, whose value is examined in
// the callee instance. A deeper projection preserves known-ness — a
// projection of an invocation-known value is invocation-known, and a
// projection of a mapped-call output keeps the fork-count outer dimension. A
// whole-outs ref ("") requires every return binding known.
func (e *knownEval) returnKnown(sub *knownInst, output string, lenOnly bool) bool {
	if output == "" {
		if len(sub.p.Returns) == 0 {
			return false
		}

		for _, b := range sub.p.Returns {
			if !e.known(sub, b.Value, lenOnly) {
				return false
			}
		}

		return true
	}

	field, _, _ := strings.Cut(output, ".")

	for _, b := range sub.p.Returns {
		if b.Param == field {
			return e.known(sub, b.Value, lenOnly)
		}
	}

	return false
}

// classNames names every (pipeline, class): class 0 keeps the pipeline's
// name, each further class gets a fresh <name>_ctx<n>. It returns nil when no
// pipeline has more than one class (nothing to split).
func (e *knownEval) classNames() map[string]map[int]string {
	split := false
	for _, ids := range e.ids {
		split = split || len(ids) > 1
	}

	if !split {
		return nil
	}

	names := map[string]map[int]string{}

	for _, in := range e.insts {
		base := in.p.Name
		if names[base] == nil {
			names[base] = map[int]string{}
		}

		switch _, named := names[base][in.id]; {
		case named:
		case in.id == 0:
			names[base][in.id] = base
		default:
			names[base][in.id] = e.freshName(base)
		}
	}

	return names
}

// freshName returns the first <base>_ctx<n> (n >= 2) that names no pipeline,
// stage, struct, or earlier copy. The choice depends only on the program, so
// the emitter and internal/overrides derive the same names.
func (e *knownEval) freshName(base string) string {
	if e.taken == nil {
		e.taken = map[string]bool{}
	}

	for n := 2; ; n++ {
		cand := base + "_ctx" + strconv.Itoa(n)

		_, pipe := e.prog.Pipelines[cand]
		_, stage := e.prog.Stages[cand]
		_, strct := e.prog.Structs[cand]

		if !pipe && !stage && !strct && !e.taken[cand] {
			e.taken[cand] = true

			return cand
		}
	}
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
