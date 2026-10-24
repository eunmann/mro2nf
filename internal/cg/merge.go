package cg

import (
	"errors"
	"fmt"
	"slices"

	"github.com/martian-lang/martian/martian/syntax"
)

var errMergeAcrossForks = errors.New("mro2nf does not resolve this merge across an outer map call's forks")

// mergeIndices lists what a merge of e.Call gathers at fork, as mrp's
// getParts lists it (core/resolve.go:348). A merge Martian expanded per
// fork of an outer map call of known length is one element per outer fork,
// whose constraint (its fork node, or its value when that is a reference to
// the merged call) fixes that fork's index while fork lacks it: mrp gathers
// the call's forks at that outer fork alone (core/resolve.go:459
// ForkId.constrain). An unconstrained merge whose references fix an outer
// fork's index, of a call whose fork count Martian does not know at compile
// time, mrp would gather across every fork of the outer call; it fails
// instead, as no program Martian 50ad6c0 compiles was found to hold one
// (its findMergeForkNode, syntax/merge_exp.go:326, sets the fork node of
// every merge whose value reads a stage forked on the call).
func (r *resolver) mergeIndices(e *syntax.MergeExp, fork Fork) ([]syntax.CollectionIndex, error) {
	if !r.g.mergeCounts(e) {
		return forklessIndices(e, r.g.boundRoots(e), fork)
	}

	refs := e.Value.FindRefs()
	constraint := mergeConstraint(e)

	if constraint != nil {
		refs = []*syntax.RefExp{constraint}
	}

	at, missing := r.mergeOuter(e, fork, refs)

	if _, known := knownIndices(e.Call.Split().Source); len(missing) > 0 && constraint == nil && !known {
		return nil, fmt.Errorf("%w: an unconstrained merge of %s across the forks of %s", errMergeAcrossForks, e.Call.Fqid, missing[0].Fqid)
	}

	return r.indices(e.Call, at)
}

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
// wherever it resolves (forklessIndices), so nothing it binds is read.
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

// forklessIndices lists what getParts gathers for a merge of e.Call whose
// bound node, of fork roots roots, has none on e.Call (core/resolve.go:
// 519-600): the call's compile-time forks where Martian knows them, and
// otherwise none, as getParts trims every part over several roots and
// finds none over roots fork matches (core/fork.go:1338
// getUnmatchedForkParts). It fails over a single root fork leaves
// unmatched, whose forks mrp would gather in place of e.Call's.
func forklessIndices(e *syntax.MergeExp, roots syntax.ForkRootList, fork Fork) ([]syntax.CollectionIndex, error) {
	if idx, ok := knownIndices(e.Call.Call()); ok {
		return idx, nil
	}

	if len(roots) == 1 {
		if _, ok := fork.part(roots[0].Call()); !ok {
			return nil, fmt.Errorf("%w: merge of %s gathers the forks of %s", errShape, e.Call.Fqid, roots[0].Fqid)
		}
	}

	return nil, nil
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

// mergeOuter is fork with the index of each outer root of the merged call
// that fork lacks and one of refs fixes, and those roots.
func (r *resolver) mergeOuter(e *syntax.MergeExp, fork Fork, refs []*syntax.RefExp) (Fork, syntax.ForkRootList) {
	out := fork

	var missing syntax.ForkRootList

	for _, o := range r.g.outerRoots(e.Call) {
		if _, ok := out.rootPart(o); ok {
			continue
		}

		for _, ref := range refs {
			if ix, ok := ref.Forks[o.Call()]; ok && ix != nil && ix.IndexSource() == nil {
				out = out.with(Part{Root: o, Index: concrete(ix)})
				missing = append(missing, o)

				break
			}
		}
	}

	return out, missing
}
