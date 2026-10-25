package cg

import (
	"slices"

	"github.com/martian-lang/martian/martian/syntax"
)

// mergeCounts reports whether mrp gathers a merge of e.Call from e.Call's
// own forks: whether the node it binds the merge to has a fork root on
// e.Call, as getParts reads the parts from that node's fork table
// (core/resolve.go:504).
func (g *Graph) mergeCounts(e *syntax.MergeExp) bool {
	return slices.ContainsFunc(g.boundRoots(e), func(o *syntax.CallGraphStage) bool {
		return o.Call() == e.Call.Call()
	})
}

// mergeGathersNone reports whether a merge of e.Call gathers no fork
// wherever it resolves, so nothing it binds is read: getParts finds no part
// of a call its bound node has no root on, unless Martian knows the call's
// length (core/resolve.go:519-600).
func (g *Graph) mergeGathersNone(e *syntax.MergeExp) bool {
	return !g.mergeCounts(e) && !e.Call.Call().KnownLength()
}

// boundRoots are the fork roots of the node mrp binds a merge of e.Call to
// (core/resolve.go:330-345): its constraint's stage, else e.Call's own
// node. A mapped sub-pipeline's roots are the merges over its outputs
// (syntax/resolve_pipeline.go:341 unmergeExp), so one whose outputs no fork
// of its own reaches has none on itself.
func (g *Graph) boundRoots(e *syntax.MergeExp) syntax.ForkRootList {
	if c := mergeConstraint(e); c != nil {
		if st, ok := g.stages[c.Id]; ok {
			return st.ForkRoots()
		}
	}

	return e.Call.ForkRoots()
}

// mergeConstraint is the reference whose static fork indices a merge of
// e.Call is constrained to (core/resolve.go:330-341): its fork node, or its
// value when that is a reference to the merged call itself.
func mergeConstraint(e *syntax.MergeExp) *syntax.RefExp {
	if e.ForkNode != nil {
		return e.ForkNode
	}

	if ref, ok := e.Value.(*syntax.RefExp); ok && ref.Id == e.Call.Fqid {
		return ref
	}

	return nil
}
