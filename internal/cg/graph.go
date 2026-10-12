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
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/eunmann/mro2nf/internal/frontend"
	"github.com/eunmann/mro2nf/internal/types"
	"github.com/martian-lang/martian/martian/core"
	"github.com/martian-lang/martian/martian/syntax"
)

var (
	errNoCall      = errors.New("no top-level call")
	errUnknownNode = errors.New("unknown call graph node")
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
		decls, _, err := splitEntry(raw, entry)
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

	root, err := ast.MakePipelineCallGraph("", ast.Call)
	if err != nil {
		return nil, fmt.Errorf("call graph of %s: %w", name, err)
	}

	g := &Graph{
		ast:      ast,
		mroPaths: mroPaths,
		root:     root,
		types:    &ast.TypeTable,
		stages:   map[string]*syntax.CallGraphStage{},
		roots:    map[string]*syntax.CallGraphStage{},
		outer:    map[string]syntax.ForkRootList{},
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
			g.addOuter(r, st.ForkRoots()[:i])
		}
	}

	return g, nil
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

// Manifest is the type manifest of the invocation's declarations, the one
// the transpiler builds from the same source (types.BuildManifest).
func (g *Graph) Manifest() types.Manifest {
	return types.BuildManifest(frontend.Lower(g.ast))
}

// MroPaths are the include paths the invocation compiled with, mrp's
// MROPATH (cmd/mrp/configure.go:247).
func (g *Graph) MroPaths() []string { return g.mroPaths }

// Stage returns the stage node fqid.
func (g *Graph) Stage(fqid string) (*syntax.CallGraphStage, error) {
	st, ok := g.stages[fqid]
	if !ok {
		return nil, fmt.Errorf("stage %s: %w", fqid, errUnknownNode)
	}

	return st, nil
}

// Producer returns the stage whose outs decide root's fork set at run time,
// the one stage its split source reads, and false for a root whose forks the
// call graph already fixes: a source of known length, or one that reads no
// stage. mrp expands such a root once that stage completes
// (core/fork.go:1576 expandForkFromRef).
func (g *Graph) Producer(root *syntax.CallGraphStage) (string, bool) {
	sp := root.Split()
	if sp == nil || sp.Source.KnownLength() {
		return "", false
	}

	refs := refIDs(sp.Value)
	if len(refs) != 1 {
		return "", false
	}

	return refs[0], true
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

// runtimeMultiSource reports a root whose fork set reads several stages: no
// one stage's post can write its forks.
func runtimeMultiSource(root *syntax.CallGraphStage) bool {
	sp := root.Split()

	return sp != nil && !sp.Source.KnownLength() && len(refIDs(sp.Value)) > 1
}

// splitCalls adds the map calls e is forked on to out, as Martian's
// unexported findSplitCalls finds a stage's fork roots in its inputs
// (syntax/resolve_stage.go:791): every split's call, and each call a ref is
// forked on at an index not yet fixed, less the calls a merge gathers.
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
