// Package cg resolves the values of stage instances on Martian's compiled call
// graph (syntax.MakePipelineCallGraph): each instance's _args, its disabled
// state, its fork set, and the pipeline outs. It mirrors mrp's runtime
// resolution (martian/core resolve.go, fork.go and stage.go at the commit
// go.mod pins) through Martian's exported API, and it is the one Go path that
// builds a stage instance's _args.
package cg

import (
	"errors"
	"fmt"
	"iter"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/eunmann/mro2nf/internal/types"
	"github.com/martian-lang/martian/martian/core"
	"github.com/martian-lang/martian/martian/syntax"
)

var (
	errNoCall      = errors.New("no top-level call")
	errUnknownNode = errors.New("unknown call graph node")
	// errCompilerPanic is a panic of Martian's compiler: its parser or its
	// call-graph builder.
	errCompilerPanic = errors.New("martian compiler panicked")
)

// Graph is a compiled invocation's call graph, indexed by stage and by fork
// root. A fork root can be a pipeline's map call, which is no stage node.
// src is the invocation source it compiled from, as the file at name.
type Graph struct {
	src, name string
	ast       *syntax.Ast
	mroPaths  []string
	root      *syntax.CallGraphPipeline
	types     *syntax.TypeLookup
	stages    map[string]*syntax.CallGraphStage
	roots     map[string]*syntax.CallGraphStage
	outer     map[string]syntax.ForkRootList
	// splitRoots are the fork roots by their own split, which two
	// instances of one sub-pipeline, sharing their calls, do not share.
	splitRoots map[*syntax.SplitExp]*syntax.CallGraphStage
	counts     map[*syntax.SplitExp]rootCount
	partnered  map[string]nodePartners
}

// Load compiles the entry .mro at entry as mrp compiles the invocation it
// reads under the path it was given (cmd/mrp/main.go:
// ReadFile(invocationPath), then core/runtime.go instantiatePipeline:
// ParseSourceBytes, then MakePipelineCallGraph), with call, when set, in
// place of the entry's own call: the run's call PLAN rebuilt
// (Source.RebuildCall) over the entry's declarations is the invocation
// PLAN compiled.
func Load(entry, call string, mroPaths []string) (*Graph, error) {
	if len(mroPaths) == 0 {
		mroPaths = []string{filepath.Dir(entry)}
	}

	raw, err := os.ReadFile(entry)
	if err != nil {
		return nil, fmt.Errorf("read invocation: %w", err)
	}

	if call != "" {
		decls, _, err := splitEntry(raw, entry, mroPaths)
		if err != nil {
			return nil, err
		}

		raw = []byte(withCall(decls, call))
	}

	return Compile(raw, entry, mroPaths)
}

// Compile compiles the invocation source src as Load compiles a file named
// name (core/runtime.go instantiatePipeline: ParseSourceBytes, then
// MakePipelineCallGraph).
func Compile(src []byte, name string, mroPaths []string) (*Graph, error) {
	var p syntax.Parser

	_, _, ast, err := p.ParseSourceBytes(src, name, mroPaths, false)
	if err != nil {
		return nil, fmt.Errorf("compile %s: %w", name, err)
	}

	g, err := graphOf(ast, name, mroPaths)
	if err != nil {
		return nil, err
	}

	g.src, g.name = string(src), name

	return g, nil
}

func graphOf(ast *syntax.Ast, name string, mroPaths []string) (*Graph, error) {
	if ast.Call == nil {
		return nil, fmt.Errorf("compile %s: %w", name, errNoCall)
	}

	root, err := callGraph(ast, name)
	if err != nil {
		return nil, err
	}

	g := &Graph{
		ast:      ast,
		mroPaths: mroPaths,
		root:     root,
		types:    &ast.TypeTable,
		stages:   map[string]*syntax.CallGraphStage{},
		roots:    map[string]*syntax.CallGraphStage{},
		outer:    map[string]syntax.ForkRootList{},

		splitRoots: map[*syntax.SplitExp]*syntax.CallGraphStage{},
		counts:     map[*syntax.SplitExp]rootCount{},
	}

	closure := root.NodeClosure()

	for _, fqid := range slices.Sorted(maps.Keys(closure)) {
		st, ok := closure[fqid].(*syntax.CallGraphStage)
		if !ok {
			continue
		}

		g.stages[fqid] = st

		for i, r := range st.ForkRoots() {
			g.roots[r.Fqid] = r
			g.splitRoots[r.Split()] = r
			g.addOuter(r, st.ForkRoots()[:i])
		}
	}

	for sp := range g.splitRoots {
		if sp != nil && !sp.Source.KnownLength() {
			g.counts[sp] = g.countSplit(sp)
		}
	}

	g.partnered = g.indexPartners()

	return g, nil
}

// callGraph is ast's call graph. Martian's builder has panicked on
// invocations mrp cannot run either (a map call over a null in a literal map,
// before 50ad6c0); a panic is that invocation's compile error, so the witness
// falls back from it as from any invocation mrp refuses.
func callGraph(ast *syntax.Ast, name string) (*syntax.CallGraphPipeline, error) {
	var (
		root *syntax.CallGraphPipeline
		err  error
	)

	if p := recovered(func() { root, err = ast.MakePipelineCallGraph("", ast.Call) }); p != nil {
		return nil, fmt.Errorf("call graph of %s: %w: %v (%T)", name, errCompilerPanic, p, p)
	}

	if err != nil {
		return nil, fmt.Errorf("call graph of %s: %w", name, err)
	}

	return root, nil
}

// recovered runs f and returns what it panicked with, nil when it returned.
func recovered(f func()) any {
	var p any

	func() {
		defer func() { p = recover() }()

		f()
	}()

	return p
}

// Invocation is the run's invocation as mrp records it in every job's
// _jobinfo: the top-level call, its bindings and its .mro file
// (core/runtime.go:352 BuildDataForAst in instantiatePipeline).
func (g *Graph) Invocation() (*core.InvocationData, error) {
	inv, err := core.BuildDataForAst(g.ast)
	if err != nil {
		return nil, fmt.Errorf("invocation data: %w", err)
	}

	return inv, nil
}

// Table is the file-leaf walk over the invocation's compiled types.
func (g *Graph) Table() *types.Table { return types.NewTable(g.types) }

// Params are the callable's parameters of role (types.Params).
func (g *Graph) Params(callable, role string) ([]*syntax.StructMember, error) {
	c := g.ast.Callables.Table[callable]
	if c == nil {
		return nil, fmt.Errorf("%w: %q", errNoCallable, callable)
	}

	ps, err := types.Params(c, role)
	if err != nil {
		return nil, fmt.Errorf("params of %s: %w", callable, err)
	}

	return ps, nil
}

// MroPaths are the include paths the invocation compiled with, mrp's
// MROPATH (cmd/mrp/configure.go:247).
func (g *Graph) MroPaths() []string { return g.mroPaths }

// Stages are the call graph's stage nodes, in no order.
func (g *Graph) Stages() iter.Seq[*syntax.CallGraphStage] { return maps.Values(g.stages) }

// Stage returns the stage node fqid.
func (g *Graph) Stage(fqid string) (*syntax.CallGraphStage, error) {
	st, ok := g.stages[fqid]
	if !ok {
		return nil, fmt.Errorf("stage %s: %w", fqid, errUnknownNode)
	}

	return st, nil
}

// Producer returns the stage whose outs decide root's fork set at run time,
// the one stage rootCountRefs finds, and false for a root whose forks the
// call graph already fixes: a source of known length, or one whose count
// reads no stage. mrp expands such a root once that stage completes
// (core/fork.go:1582 expandForkFromRef).
func (g *Graph) Producer(root *syntax.CallGraphStage) (string, bool) {
	refs := g.rootCountRefs(root)
	if len(refs) != 1 {
		return "", false
	}

	return refs[0], true
}

// rootCountRefs are the stages whose outs fix root's fork count: none for a
// split source of known length, else those its compiled source reads where
// mrp expands the root's forks from it.
func (g *Graph) rootCountRefs(root *syntax.CallGraphStage) []string {
	return g.rootCount(root).stages
}

// rootCount is what counting a root's forks reads, as mrp expands them from
// its compiled source (countWalk): the stages whose outs fix the count, the
// disable gates it evaluates on the way, and every reference it reads, the
// gates' too.
type rootCount struct {
	stages []string
	gates  []syntax.Exp
	reads  []*syntax.RefExp
}

// rootCount is root's, none for a source of known length. Every stage's
// fork roots are counted once, when the graph is built; the call of a merge
// no stage is forked on, such as a mapped sub-pipeline's in the pipeline
// outs, is counted where it is asked for.
func (g *Graph) rootCount(root *syntax.CallGraphStage) rootCount {
	sp := root.Split()
	if sp == nil || sp.Source.KnownLength() {
		return rootCount{}
	}

	if c, ok := g.counts[sp]; ok {
		return c
	}

	return g.countSplit(sp)
}

func (g *Graph) countSplit(sp *syntax.SplitExp) rootCount {
	w := countWalk{g: g, sp: sp}
	stages := w.source(sp.Source)
	c := rootCount{stages: stages, gates: w.gates, reads: w.refs}

	for _, gate := range w.gates {
		c.reads = append(c.reads, gate.FindRefs()...)
	}

	return c
}

// countWalk walks the compiled source of split sp case for case as mrp
// expands its forks from it (core/fork.go:1412 expandForkPartFromSource,
// 1441 expandForkPartFromExp, 1491 expandForkPartFromSplit), returning the
// stages whose outs fix the fork count and collecting in gates the disable
// gates mrp evaluates on the way and in refs the references it reads.
type countWalk struct {
	g     *Graph
	sp    *syntax.SplitExp
	gates []syntax.Exp
	refs  []*syntax.RefExp
}

func (w *countWalk) source(src syntax.MapCallSource) []string {
	switch s := src.(type) {
	case *syntax.MergeExp:
		if s.ForkNode != nil {
			return w.ref(s.ForkNode)
		}

		return w.source(s.MergeOver)
	case *syntax.MapCallSet:
		return w.source(s.Master)
	case syntax.Exp:
		return w.exp(s)
	case *syntax.BoundReference:
		return w.ref(s.Exp)
	default:
		return nil
	}
}

// exp counts from the value e: a literal's length reads no stage, whatever
// its elements read, and a split over another call's forks counts the
// elements it splits into. Of a disable-wrapped value mrp reads its gate
// and its value; only its value's stages count when the gate disables each
// of them too, since only an instance that runs posts, which holds the gate
// false.
func (w *countWalk) exp(e syntax.Exp) []string {
	switch x := e.(type) {
	case *syntax.RefExp:
		return w.ref(x)
	case *syntax.MergeExp:
		return w.source(x)
	case *syntax.DisabledExp:
		if d, ok := x.Disabled.(*syntax.SplitExp); ok && d.Call == w.sp.Call {
			return w.exp(d.Value)
		}

		gate := refIDs(x.Disabled)
		if len(gate) > 0 {
			w.gates = append(w.gates, x.Disabled)
		}

		inner := w.exp(x.Value)
		if len(inner) > 0 && w.g.allGatedBy(inner, x.Disabled) {
			return inner
		}

		return union(gate, inner)
	case *syntax.SplitExp:
		if x.Call == w.sp.Call {
			return w.exp(x.Value)
		}

		return w.elements(x.Value)
	default:
		return nil
	}
}

// elements counts the collections a split over another call's forks splits
// e into (core/fork.go:1532 expandForkSplitInnerPart): each element of a
// literal, or else e's.
func (w *countWalk) elements(e syntax.Exp) []string {
	var out []string

	switch x := e.(type) {
	case *syntax.ArrayExp:
		for _, v := range x.Value {
			out = union(out, w.exp(v))
		}
	case *syntax.MapExp:
		for _, k := range slices.Sorted(maps.Keys(x.Value)) {
			out = union(out, w.exp(x.Value[k]))
		}
	default:
		out = w.exp(e)
	}

	return out
}

// ref is the stage a reference reads, none for an entry input, and records
// the reference among those the count reads.
func (w *countWalk) ref(ref *syntax.RefExp) []string {
	if ref == nil || ref.Kind == syntax.KindSelf {
		return nil
	}

	w.refs = append(w.refs, ref)

	return []string{ref.Id}
}

// countReads are the stage outs counting root's forks reads (rootCount).
func (g *Graph) countReads(root *syntax.CallGraphStage) []*syntax.RefExp {
	return g.rootCount(root).reads
}

// allGatedBy reports whether every stage of fqids has gate among its
// disable bindings.
func (g *Graph) allGatedBy(fqids []string, gate syntax.Exp) bool {
	ref, ok := gate.(*syntax.RefExp)
	if !ok {
		return false
	}

	for _, id := range fqids {
		st, ok := g.stages[id]
		if !ok || !slices.ContainsFunc(st.Disabled(), func(d syntax.Exp) bool {
			dr, ok := d.(*syntax.RefExp)

			return ok && sameRef(ref, dr)
		}) {
			return false
		}
	}

	return true
}

// sourceGates are the disable gates mrp evaluates expanding the forks of
// every root whose forks it expands at run time (countWalk), which it
// requires ready to restore those forks when it reattaches a pipestance
// (core/pipestance.go RestoreForks, core/fork.go getDisabledSource).
func (g *Graph) sourceGates() []syntax.Exp {
	out := make([]syntax.Exp, 0, len(g.roots))

	for _, root := range g.sortedRoots() {
		out = append(out, g.rootCount(root).gates...)
	}

	return out
}

// union is a with each of b it lacks appended.
func union(a, b []string) []string {
	for _, s := range b {
		if !slices.Contains(a, s) {
			a = append(a, s)
		}
	}

	return a
}

func (g *Graph) forkRoot(fqid string) (*syntax.CallGraphStage, error) {
	r, ok := g.roots[fqid]
	if !ok {
		return nil, fmt.Errorf("fork root %s: %w", fqid, errUnknownNode)
	}

	return r, nil
}

// addOuter adds to root's outer roots those of before, the fork roots ahead
// of root in a stage's fork, that root's fork set depends on. Every stage's
// fork orders the roots it shares with another stage alike, so the stages
// agree on the order, and visiting them sorted fixes it when one stage lacks
// a root another has.
func (g *Graph) addOuter(root *syntax.CallGraphStage, before syntax.ForkRootList) {
	sp := root.Split()
	if sp == nil || sp.Source.KnownLength() {
		return
	}

	deps := map[*syntax.CallStm]bool{}
	splitCalls(sp.Value, deps)

	for _, o := range before {
		if deps[o.Call()] && !slices.ContainsFunc(g.outer[root.Fqid], func(c *syntax.CallGraphStage) bool { return c.Fqid == o.Fqid }) {
			g.outer[root.Fqid] = append(g.outer[root.Fqid], o)
		}
	}
}

// outerRoots are the map calls root's fork set varies with, outermost
// first: those its split source is forked on, none when the source's length
// is known. A root's fork elements are keyed by these and root alone,
// whatever else a stage forked on root is forked on.
func (g *Graph) outerRoots(root *syntax.CallGraphStage) syntax.ForkRootList {
	return g.outer[root.Fqid]
}

// splitCalls adds the map calls e is forked on to out, as Martian's
// unexported findSplitCalls finds a stage's fork roots in its inputs
// (syntax/resolve_stage.go:789): every split's call, and each call a ref is
// forked on at an index not yet fixed, less the calls a merge gathers.
// TestSplitCallsAreMartians holds it to the fork roots Martian compiles.
func splitCalls(e syntax.Exp, out map[*syntax.CallStm]bool) {
	switch x := e.(type) {
	case *syntax.SplitExp:
		out[x.Call] = true
		splitCalls(x.Value, out)
	case *syntax.MergeExp:
		had := out[x.GetCall()]
		splitCalls(x.Value, out)

		if !had {
			delete(out, x.GetCall())
		}
	case *syntax.ArrayExp:
		for _, v := range x.Value {
			splitCalls(v, out)
		}
	case *syntax.MapExp:
		for _, v := range x.Value {
			splitCalls(v, out)
		}
	case *syntax.DisabledExp:
		splitCalls(x.Value, out)
		splitCalls(x.Disabled, out)
	case *syntax.RefExp:
		for c, ix := range x.Forks {
			if ix != nil && ix.IndexSource() != nil {
				out[c] = true
			}
		}
	}
}

func refIDs(e syntax.Exp) []string {
	seen := map[string]bool{}

	var out []string

	for _, r := range e.FindRefs() {
		if !seen[r.Id] {
			seen[r.Id] = true
			out = append(out, r.Id)
		}
	}

	return out
}
