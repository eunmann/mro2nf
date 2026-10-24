package cg

import (
	"errors"
	"fmt"

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
