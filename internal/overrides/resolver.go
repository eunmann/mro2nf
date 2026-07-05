package overrides

import (
	"slices"
	"sort"
	"strings"

	"github.com/eunmann/mro2nf/internal/ir"
)

// callRef locates one call site of a stage: the pipeline containing the call
// and the call instance name — the two axes embedded in the fused/scatter
// process names (STAGE_<n>_<pipe>__<call>, FORK_<n>_<pipe>__<call>). A
// bare-stage entry has no containing pipeline (empty pipe): the emitter names
// it a plain <stage> process, never a fused one.
type callRef struct{ pipe, call string }

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
	// stageRefs maps a callable (stage) name to the sorted unique call sites
	// that invoke it anywhere in the program.
	stageRefs map[string][]callRef
}

// newResolver builds a resolver over prog (may be nil).
func newResolver(prog *ir.Program) *resolver {
	r := &resolver{
		prog: prog, stage: map[string]bool{}, leaves: map[string][]string{},
		callStage: map[string]string{}, stageRefs: map[string][]callRef{},
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
			r.markStageCall("", prog.Entry.Callable, prog.Entry.Callable)
		}
	}

	for stage, refs := range r.stageRefs {
		slices.SortFunc(refs, func(a, b callRef) int {
			if c := strings.Compare(a.pipe, b.pipe); c != 0 {
				return c
			}

			return strings.Compare(a.call, b.call)
		})
		r.stageRefs[stage] = slices.Compact(refs)
	}

	return r
}

// markStageCall records that call name invokes stage callable inside pipeline
// pipe ("" for a bare-stage entry).
func (r *resolver) markStageCall(pipe, call, callable string) {
	r.stage[call] = true
	r.stage[callable] = true
	r.callStage[call] = callable
	r.stageRefs[callable] = append(r.stageRefs[callable], callRef{pipe, call})
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
			r.markStageCall(pipeName, c.Name, c.Callable)

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

// targets returns the leaf-STAGE (callable) names an override key maps to and
// how specifically (kind), plus a note that is non-empty when the key names
// nothing the pipeline emits (so the caller reports it instead of silently
// emitting a dead selector). The empty key "" is the global default. A key
// resolves to the stage(s) it touches — an alias key to the aliased call's
// stage, a pipeline key to the leaf stages beneath it — so precedence and the
// resolved-map dedup stay per (stage, phase, field) exactly as before; the
// selector then covers ALL of that stage's call sites (see callsFor). Without a
// program, the key's last segment is returned unchanged (the conservative
// legacy behavior — the emitter's call name equalled the stage name before
// per-call fusion/scatter existed).
func (r *resolver) targets(key string) ([]string, int, string) {
	seg := lastSegment(key)
	if seg == "" {
		return []string{""}, kindGlobal, "" // global process default
	}

	if r.prog == nil {
		return []string{seg}, kindStage, ""
	}

	if r.stage[seg] {
		return []string{r.callableOf(seg)}, kindStage, ""
	}

	if lv, ok := r.leaves[seg]; ok {
		if len(lv) == 0 {
			return nil, kindExpand, "names a sub-pipeline with no stages"
		}

		return r.stagesOf(lv), kindExpand, ""
	}

	return nil, kindStage, "no stage or sub-pipeline of that name in the pipeline"
}

// callableOf maps a leaf-stage segment to its callable (stage) name: a callable
// name is itself; a call instance name maps to its callable.
func (r *resolver) callableOf(seg string) string {
	if r.prog.Stages[seg] != nil {
		return seg
	}

	return r.callStage[seg]
}

// stagesOf maps a sub-pipeline's leaf call names to their distinct, sorted
// callable (stage) names.
func (r *resolver) stagesOf(calls []string) []string {
	stages := make([]string, 0, len(calls))
	for _, c := range calls {
		stages = append(stages, r.callStage[c])
	}

	return dedupSorted(stages)
}

// qualifiers returns the fused/scatter name-qualifier fragments for stage —
// one `[0-9]+_<pipe>__<callAlt>` per pipeline that calls the stage, covering
// every call site so a stage-level override reaches an aliased scattered
// call. The pipeline names are LITERAL: Martian identifiers may contain "__",
// so a `.+` qualifier would let call TRIM's selector absorb the qualifier of
// an unrelated call X__TRIM (`.+` matching "PIPE__X"); a literal pipeline
// name cannot. Without a program the single `.+`-qualified fragment is
// returned — structurally ambiguous for such names (see the package doc). A
// bare-stage entry contributes nothing: its process is plain-named, never
// fused. `[0-9]` rather than `\d`: the selector is rendered inside a
// single-quoted Groovy string, where a backslash escape fails config parsing.
func (r *resolver) qualifiers(stage string) []string {
	if r.prog == nil {
		return []string{`[0-9]+_.+__` + stage}
	}

	byPipe := map[string][]string{}

	for _, ref := range r.stageRefs[stage] {
		if ref.pipe != "" {
			byPipe[ref.pipe] = append(byPipe[ref.pipe], ref.call)
		}
	}

	quals := make([]string, 0, len(byPipe))
	for _, pipe := range sortedKeys(byPipe) {
		quals = append(quals, `[0-9]+_`+pipe+`__`+altGroup(byPipe[pipe]))
	}

	return quals
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
