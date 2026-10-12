package cg

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/martian-lang/martian/martian/syntax"
)

var (
	errMergeUnmatched   = errors.New("could not match reference to a specific fork")
	errMergeAcrossForks = errors.New("mro2nf does not resolve this merge across an outer map call's forks")
)

// mergeParts are the indices a merge outside its call's forks gathers.
// list is what it gathers, in order; own is the call's indices at the
// element's own outer fork. Across an outer map call's forks (across), a
// nil index in list stands for an outer fork with no fork of the call.
type mergeParts struct {
	list   []syntax.CollectionIndex
	own    []syntax.CollectionIndex
	across bool
}

// mergeParts lists what a merge of e.Call gathers at fork, as mrp's getParts
// lists it (core/resolve.go:444). A merge Martian expanded per fork of an
// outer map call of known length is one element per outer fork, whose
// references fix that fork's index (mergeOuter) while fork lacks it. Unless
// the call's split source has a length Martian knows at compile time, mrp
// then lists the call's part of every fork of the call under fork, across
// every fork of the outer call (acrossOuter), and resolves each at the
// element's own outer fork (mergeElement). With one outer fork that is the
// element's own forks (gen-diff seed 5).
func (r *resolver) mergeParts(e *syntax.MergeExp, fork Fork) (mergeParts, error) {
	at, missing := r.mergeOuter(e, fork)

	own, err := r.indices(e.Call, at)
	if err != nil {
		return mergeParts{}, err
	}

	same := mergeParts{list: own, own: own}
	if len(missing) == 0 {
		return same, nil
	}

	if _, known := knownIndices(e.Call.Split().Source); known {
		return same, nil
	}

	if len(missing) > 1 {
		return same, r.singleForks(missing, at)
	}

	list, n, err := r.acrossOuter(e.Call, fork, missing[0])
	if err != nil || n <= 1 {
		return same, err
	}

	if e.CallMode() == syntax.ModeMapCall {
		return mergeParts{}, fmt.Errorf("%w: a merge of map call %s across %d forks of %s", errMergeAcrossForks, e.Call.Fqid, n, missing[0].Fqid)
	}

	return mergeParts{list: list, own: own, across: true}, nil
}

// singleForks fails unless each of roots has one fork at fork: only then
// does a merge across several outer map calls gather the element's own
// forks.
func (r *resolver) singleForks(roots syntax.ForkRootList, fork Fork) error {
	for _, o := range roots {
		idx, err := r.indices(o, fork)
		if err != nil {
			return err
		}

		if len(idx) > 1 {
			return fmt.Errorf("%w: a merge across %d outer map calls, %s of %d forks", errMergeAcrossForks, len(roots), o.Fqid, len(idx))
		}
	}

	return nil
}

// mergeOuter is fork with the index of each outer root of the merged call
// that fork lacks and the merged value's references fix, and those roots.
func (r *resolver) mergeOuter(e *syntax.MergeExp, fork Fork) (Fork, syntax.ForkRootList) {
	out := fork

	var missing syntax.ForkRootList

	for _, o := range r.g.outerRoots(e.Call) {
		if _, ok := out.rootPart(o); ok {
			continue
		}

		for _, ref := range e.Value.FindRefs() {
			if ix, ok := ref.Forks[o.Call()]; ok && ix != nil && ix.IndexSource() == nil {
				out = out.with(Part{Root: o, Index: concrete(ix)})
				missing = append(missing, o)

				break
			}
		}
	}

	return out, missing
}

// acrossOuter lists call's part of every fork of call under fork across the
// forks of o, in mrp's fork order, and how many forks o has: each fork of
// o's first fork of call, then each one's others, as Node.expandForks
// expands a fork in place and appends its further forks
// (core/node.go:479). An outer fork with no fork of call lists one nil. A
// list is computed once per merge and fork, not once per outer fork.
func (r *resolver) acrossOuter(call *syntax.CallGraphStage, fork Fork, o *syntax.CallGraphStage) ([]syntax.CollectionIndex, int, error) {
	key := call.Fqid + "\x00" + o.Fqid + "\x00" + fork.key()
	if l, ok := r.across[key]; ok {
		return l.list, l.n, nil
	}

	outer, err := r.indices(o, fork)
	if err != nil {
		return nil, 0, err
	}

	var firsts, rests []syntax.CollectionIndex

	for _, i := range outer {
		s, err := r.indices(call, fork.with(Part{Root: o, Index: i}))
		if err != nil {
			return nil, 0, err
		}

		if len(s) == 0 {
			firsts = append(firsts, nil)

			continue
		}

		firsts = append(firsts, s[0])
		rests = append(rests, s[1:]...)
	}

	if r.across == nil {
		r.across = map[string]acrossList{}
	}

	l := acrossList{list: append(firsts, rests...), n: len(outer)}
	r.across[key] = l

	return l.list, l.n, nil
}

// acrossList is one acrossOuter result.
type acrossList struct {
	list []syntax.CollectionIndex
	n    int
}

// mergeElement is the merged value at ix. Across an outer map call's forks
// it is null at a nil index and at every index when the element's own outer
// fork has no fork of the call, and an index that fork lacks fails the
// merge, as real mrp runs read them (merge_across_outer,
// merge_across_empty, map_pipe_nested).
func (r *resolver) mergeElement(e *syntax.MergeExp, t syntax.Type, fork Fork, parts mergeParts, ix syntax.CollectionIndex) (json.Marshaler, error) {
	if !parts.across {
		return r.resolve(e.Value, t, fork.with(Part{Root: e.Call, Index: ix}))
	}

	if ix == nil || len(parts.own) == 0 {
		if _, ok := e.Value.(*syntax.RefExp); !ok {
			return nil, fmt.Errorf("%w: a %T at an empty fork of %s", errMergeAcrossForks, e.Value, e.Call.Fqid)
		}

		return nil, nil
	}

	if !slices.ContainsFunc(parts.own, func(o syntax.CollectionIndex) bool { return indexEqual(o, ix) }) {
		return nil, fmt.Errorf("%w of %s: its element's outer fork has no fork at %s", errMergeUnmatched, e.Call.Fqid, ix.GoString())
	}

	return r.resolve(e.Value, t, fork.with(Part{Root: e.Call, Index: ix}))
}
