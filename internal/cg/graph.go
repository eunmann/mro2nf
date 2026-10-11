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
	"path/filepath"

	"github.com/martian-lang/martian/martian/syntax"
)

var (
	errNoCall      = errors.New("no top-level call")
	errUnknownNode = errors.New("unknown call graph node")
)

// Graph is a compiled invocation's call graph, indexed by stage and by fork
// root. A fork root can be a pipeline's map call, which is no stage node.
type Graph struct {
	root   *syntax.CallGraphPipeline
	types  *syntax.TypeLookup
	stages map[string]*syntax.CallGraphStage
	roots  map[string]*syntax.CallGraphStage
	outer  map[string]syntax.ForkRootList
}

// Load compiles the invocation at mro, as mrp does before it creates a
// pipestance (core/runtime.go instantiatePipeline: Compile, then
// MakePipelineCallGraph).
func Load(mro string, mroPaths []string) (*Graph, error) {
	if len(mroPaths) == 0 {
		mroPaths = []string{filepath.Dir(mro)}
	}

	_, _, ast, err := syntax.Compile(mro, mroPaths, false)
	if err != nil {
		return nil, fmt.Errorf("compile %s: %w", mro, err)
	}

	if ast.Call == nil {
		return nil, fmt.Errorf("compile %s: %w", mro, errNoCall)
	}

	root, err := ast.MakePipelineCallGraph("", ast.Call)
	if err != nil {
		return nil, fmt.Errorf("call graph of %s: %w", mro, err)
	}

	g := &Graph{
		root:   root,
		types:  &ast.TypeTable,
		stages: map[string]*syntax.CallGraphStage{},
		roots:  map[string]*syntax.CallGraphStage{},
		outer:  map[string]syntax.ForkRootList{},
	}

	for fqid, n := range root.NodeClosure() {
		st, ok := n.(*syntax.CallGraphStage)
		if !ok {
			continue
		}

		g.stages[fqid] = st

		for i, r := range st.ForkRoots() {
			g.roots[r.Fqid] = r
			g.outer[r.Fqid] = st.ForkRoots()[:i:i]
		}
	}

	return g, nil
}

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

// outerRoots are the map calls root's forks are nested in, outermost first:
// the fork roots before it in the fork of any stage forked on it. A
// pipeline's map call keeps no fork roots of its own.
func (g *Graph) outerRoots(root *syntax.CallGraphStage) syntax.ForkRootList {
	return g.outer[root.Fqid]
}

// runtimeMultiSource reports a root whose fork set reads several stages: no
// one stage's post can write its forks.
func runtimeMultiSource(root *syntax.CallGraphStage) bool {
	sp := root.Split()

	return sp != nil && !sp.Source.KnownLength() && len(refIDs(sp.Value)) > 1
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
