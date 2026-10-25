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

// mergeGathersNone reports whether a merge of e.Call, resolved by an
// instance forked on reader, gathers no fork, so nothing it binds is read:
// getParts finds no part of a call its bound node has no root on, unless
// Martian knows the call's length or the bound node has one root the
// instance is not forked on (core/resolve.go:519-600, mergeCounted).
func (g *Graph) mergeGathersNone(e *syntax.MergeExp, reader syntax.ForkRootList) bool {
	_, ok := g.mergeCounted(e, reader)

	return !ok && !e.Call.Call().KnownLength()
}

// mergeCounted is the root whose forks a merge of e.Call, resolved by an
// instance forked on reader, gathers at run time: e.Call where its bound
// node has a root on it (mergeCounts); else, where Martian does not know
// the call's length, the bound node's one root when it has only one and
// the instance is not forked on it, whose forks getParts then takes
// (getUnmatchedForkParts, core/resolve.go:598-606); false otherwise, where
// it gathers none, or the forks Martian knows.
func (g *Graph) mergeCounted(e *syntax.MergeExp, reader syntax.ForkRootList) (*syntax.CallGraphStage, bool) {
	if g.mergeCounts(e) {
		return e.Call, true
	}

	bound := g.boundRoots(e)
	if e.Call.Call().KnownLength() || len(bound) != 1 || slices.Contains(reader, bound[0]) {
		return nil, false
	}

	return bound[0], true
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
