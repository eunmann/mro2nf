package overrides

import (
	"sort"

	"github.com/eunmann/mro2nf/internal/ir"
)

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
	// stage/call names reachable beneath it.
	leaves map[string][]string
}

// newResolver builds a resolver over prog (may be nil).
func newResolver(prog *ir.Program) *resolver {
	r := &resolver{prog: prog, stage: map[string]bool{}, leaves: map[string][]string{}}
	if prog == nil {
		return r
	}

	for name := range prog.Pipelines {
		r.collect(name, map[string]bool{})
	}

	// A bare-stage entry (top-level `call STAGE`) has no enclosing pipeline, so
	// mark it directly; the emitter still names a process for it.
	if prog.Entry != nil {
		if _, ok := prog.Stages[prog.Entry.Callable]; ok {
			r.stage[prog.Entry.Callable] = true
		}
	}

	return r
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
			r.stage[c.Name] = true
			r.stage[c.Callable] = true

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

// targets returns the stage names an override key maps to and how specifically
// (kind), plus a note that is non-empty when the key names nothing the pipeline
// emits (so the caller reports it instead of silently emitting a dead selector).
// The empty key "" is the global default. Without a program, the key's last
// segment is returned as a stage (the conservative legacy behavior).
func (r *resolver) targets(key string) ([]string, int, string) {
	seg := lastSegment(key)
	if seg == "" {
		return []string{""}, kindGlobal, "" // global process default
	}

	if r.prog == nil || r.stage[seg] {
		return []string{seg}, kindStage, ""
	}

	if lv, ok := r.leaves[seg]; ok {
		if len(lv) == 0 {
			return nil, kindExpand, "names a sub-pipeline with no stages"
		}

		return lv, kindExpand, ""
	}

	return nil, kindStage, "no stage or sub-pipeline of that name in the pipeline"
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
