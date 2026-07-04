package overrides

import (
	"sort"

	"github.com/eunmann/mro2nf/internal/ir"
)

// target is one resolved override destination: a leaf STAGE and the CALL
// instance names that invoke it. A generated project names processes two ways —
// the plain/keyed family after the STAGE (`<stage>`, `<stage>_MAP`, …) and the
// fused per-call family after the CALL (`STAGE_<n>_<pipe>__<call>`, emitted by
// #16 fusion and #76 native scatter). A selector must cover both, so an
// override reaches its stage whether a given call is fused/scattered (call-
// named) or plain/keyed (stage-named), and whether the call is aliased.
type target struct {
	stage string
	calls []string
}

// resolver maps an override key to the generated stage/call names it targets,
// using the pipeline program to expand a pipeline-scoped key to the leaf stages
// beneath it and to flag a key that names nothing. A nil program yields the
// legacy behavior: every key resolves to its own last segment, unverified.
type resolver struct {
	// nil when no program was supplied.
	prog *ir.Program
	// stage is the set of call/callable names that denote a leaf stage process.
	stage map[string]bool
	// leaves maps a sub-pipeline's call/callable name to the sorted leaf
	// call names reachable beneath it.
	leaves map[string][]string
	// callStage maps a leaf call instance name to its callable (stage) name.
	callStage map[string]string
	// stageCalls maps a callable (stage) name to the sorted unique call
	// instance names that invoke it anywhere in the program.
	stageCalls map[string][]string
}

// newResolver builds a resolver over prog (may be nil).
func newResolver(prog *ir.Program) *resolver {
	r := &resolver{
		prog: prog, stage: map[string]bool{}, leaves: map[string][]string{},
		callStage: map[string]string{}, stageCalls: map[string][]string{},
	}
	if prog == nil {
		return r
	}

	for name := range prog.Pipelines {
		r.collect(name, map[string]bool{})
	}

	// A bare-stage entry (top-level `call STAGE`) has no enclosing pipeline, so
	// mark it directly; the emitter names a plain `<stage>` process for it (no
	// per-call fused name), so the stage is its own sole "call".
	if prog.Entry != nil {
		if _, ok := prog.Stages[prog.Entry.Callable]; ok {
			r.markStageCall(prog.Entry.Callable, prog.Entry.Callable)
		}
	}

	for stage, calls := range r.stageCalls {
		r.stageCalls[stage] = dedupSorted(calls)
	}

	return r
}

// markStageCall records that call name invokes stage callable.
func (r *resolver) markStageCall(call, callable string) {
	r.stage[call] = true
	r.stage[callable] = true
	r.callStage[call] = callable
	r.stageCalls[callable] = append(r.stageCalls[callable], call)
}

// collect returns the leaf stage/call names beneath pipeline pipeName,
// memoizing into r.leaves and marking leaf stages in r.stage. inProgress guards
// against a malformed cyclic call graph (Martian forbids cycles, but the walk
// must terminate regardless).
func (r *resolver) collect(pipeName string, inProgress map[string]bool) []string {
	if cached, ok := r.leaves[pipeName]; ok {
		return cached
	}

	if inProgress[pipeName] {
		return nil
	}

	inProgress[pipeName] = true

	p := r.prog.Pipelines[pipeName]
	if p == nil {
		return nil
	}

	var out []string

	for _, c := range p.Calls {
		switch {
		case r.prog.Stages[c.Callable] != nil:
			r.markStageCall(c.Name, c.Callable)

			out = append(out, c.Name)
		case r.prog.Pipelines[c.Callable] != nil:
			sub := r.collect(c.Callable, inProgress)
			// Index the expansion under the call instance name (an aliased
			// `call SUB as X` keys on X); the callable name is indexed by the
			// recursive collect's own r.leaves[pipeName] write.
			r.leaves[c.Name] = sub

			out = append(out, sub...)
		}
	}

	out = dedupSorted(out)
	r.leaves[pipeName] = out

	return out
}

// Target kinds, ordered by how narrowly a key targets a stage. A key resolving
// directly to a leaf stage is more specific than one that expands a whole
// sub-pipeline onto that stage, which in turn beats the all-stages default — so
// an explicit stage override wins even when it has fewer path segments than a
// pipeline-scoped key that also covers the stage.
const (
	kindGlobal = iota // the "" all-stages default
	kindExpand        // expanded from a sub-pipeline key
	kindStage         // resolved directly to a leaf stage
)

// targets returns the (stage, calls) destinations an override key maps to and
// how specifically (kind), plus a note that is non-empty when the key names
// nothing the pipeline emits (so the caller reports it instead of silently
// emitting a dead selector). The empty key "" is the global default. Without a
// program, the key's last segment is returned as both stage and its own call
// (the conservative legacy behavior — the emitter's call name equalled the
// stage name before per-call fusion/scatter existed).
func (r *resolver) targets(key string) ([]target, int, string) {
	seg := lastSegment(key)
	if seg == "" {
		return []target{{stage: ""}}, kindGlobal, "" // global process default
	}

	if r.prog == nil {
		return []target{{stage: seg, calls: []string{seg}}}, kindStage, ""
	}

	if r.stage[seg] {
		return []target{r.stageTarget(seg)}, kindStage, ""
	}

	if lv, ok := r.leaves[seg]; ok {
		if len(lv) == 0 {
			return nil, kindExpand, "names a sub-pipeline with no stages"
		}

		return r.groupCalls(lv), kindExpand, ""
	}

	return nil, kindStage, "no stage or sub-pipeline of that name in the pipeline"
}

// stageTarget resolves a leaf-stage segment (either a callable name or a call
// instance name) to its (stage, calls) target. A callable name covers every
// call site of that stage (so a stage-level override reaches an aliased
// scattered call); a call name covers that one call plus its stage's shared
// plain/keyed process.
func (r *resolver) stageTarget(seg string) target {
	if r.prog.Stages[seg] != nil {
		return target{stage: seg, calls: r.stageCalls[seg]}
	}

	return target{stage: r.callStage[seg], calls: []string{seg}}
}

// groupCalls turns a sub-pipeline's leaf call names into one target per stage,
// each carrying the calls of that stage within the expansion.
func (r *resolver) groupCalls(calls []string) []target {
	byStage := map[string][]string{}
	for _, c := range calls {
		byStage[r.callStage[c]] = append(byStage[r.callStage[c]], c)
	}

	out := make([]target, 0, len(byStage))
	for _, stage := range sortedStrKeys(byStage) {
		out = append(out, target{stage: stage, calls: dedupSorted(byStage[stage])})
	}

	return out
}

// sortedStrKeys returns the sorted keys of a string-keyed map.
func sortedStrKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}

// dedupSorted returns the sorted, de-duplicated elements of in.
func dedupSorted(in []string) []string {
	if len(in) == 0 {
		return nil
	}

	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))

	for _, s := range in {
		if !seen[s] {
			seen[s] = true

			out = append(out, s)
		}
	}

	sort.Strings(out)

	return out
}
