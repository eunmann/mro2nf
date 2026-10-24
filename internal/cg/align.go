package cg

import (
	"maps"
	"slices"

	"github.com/martian-lang/martian/martian/syntax"
)

// alignment maps each root of node up that st is not forked on to the root
// of st whose forks index it alike: every reference to up in st's bindings
// sits at the element of a split over that root of st whose index is its
// index of up's root. Martian expands a split over the outs of a map call
// into such a collection, a literal holding the reference to each fork at
// that fork's index or a merge of the forks in their order, so an instance
// of st reads the one instance of up at its own index, as an mrp job reads
// it, and the driver joins it on the two roots' ordinals rather than
// gathering every instance of up. It is nil when a root of up is not so
// aligned.
func (g *Graph) alignment(st *syntax.CallGraphStage, up string) map[string]string {
	u, err := g.Stage(up)
	if err != nil {
		return nil
	}

	a := aligner{reader: st.ForkRoots(), up: up, cand: map[string][]string{}, unaligned: map[string]bool{}}

	for _, r := range u.ForkRoots() {
		if !slices.ContainsFunc(a.reader, func(x *syntax.CallGraphStage) bool { return x.Fqid == r.Fqid }) {
			a.roots = append(a.roots, r)
		}
	}

	if len(a.roots) == 0 {
		return nil
	}

	for _, e := range bindings(st) {
		a.walk(e, nil)
	}

	out := map[string]string{}

	for _, r := range a.roots {
		if a.unaligned[r.Fqid] || len(a.cand[r.Fqid]) == 0 {
			return nil
		}

		out[r.Fqid] = a.cand[r.Fqid][0]
	}

	return out
}

// alignCtx is an enclosing split over a reader root, at its element at
// idx, or at the merge of root merge's forks.
type alignCtx struct {
	root  *syntax.CallGraphStage
	idx   syntax.CollectionIndex
	keys  map[string]syntax.Exp
	merge *syntax.CallGraphStage
}

// aligner finds, for each of roots, the reader roots every reference to up
// aligns it with (cand), in reader root order, or that one does not.
type aligner struct {
	reader    syntax.ForkRootList
	up        string
	roots     syntax.ForkRootList
	cand      map[string][]string
	seen      bool
	unaligned map[string]bool
}

func (a *aligner) readerRoot(call *syntax.CallStm) *syntax.CallGraphStage {
	i := slices.IndexFunc(a.reader, func(r *syntax.CallGraphStage) bool { return r.Call() == call })
	if i < 0 {
		return nil
	}

	return a.reader[i]
}

// walk visits e under the enclosing splits ctx.
func (a *aligner) walk(e syntax.Exp, ctx []alignCtx) {
	switch x := e.(type) {
	case *syntax.RefExp:
		if x.Id == a.up {
			a.ref(x, ctx)
		}
	case *syntax.SplitExp:
		if r := a.readerRoot(x.Call); r != nil {
			a.collection(r, x.Value, ctx)
		} else {
			a.walk(x.Value, ctx)
		}
	case *syntax.ArrayExp:
		for _, v := range x.Value {
			a.walk(v, ctx)
		}
	case *syntax.MapExp:
		for _, k := range slices.Sorted(maps.Keys(x.Value)) {
			a.walk(x.Value[k], ctx)
		}
	case *syntax.MergeExp:
		a.walk(x.Value, ctx)

		if x.ForkNode != nil {
			a.walk(x.ForkNode, ctx)
		}
	case *syntax.DisabledExp:
		a.walk(x.Disabled, ctx)
		a.walk(x.Value, ctx)
	}
}

// collection visits v, the collection a split over the reader root r
// forks, element by element.
func (a *aligner) collection(r *syntax.CallGraphStage, v syntax.Exp, ctx []alignCtx) {
	switch x := v.(type) {
	case *syntax.ArrayExp:
		for i, el := range x.Value {
			a.walk(el, append(slices.Clip(ctx), alignCtx{root: r, idx: ArrayIndex(i)}))
		}
	case *syntax.MapExp:
		for _, k := range slices.Sorted(maps.Keys(x.Value)) {
			a.walk(x.Value[k], append(slices.Clip(ctx), alignCtx{root: r, idx: MapKey(k), keys: x.Value}))
		}
	case *syntax.MergeExp:
		a.walk(x.Value, append(slices.Clip(ctx), alignCtx{root: r, merge: x.Call}))
	case *syntax.DisabledExp:
		a.walk(x.Disabled, ctx)
		a.collection(r, x.Value, ctx)
	case *syntax.SplitExp:
		if o := a.readerRoot(x.Call); o != nil {
			a.nested(r, o, x.Value, ctx)
		} else {
			a.walk(x, ctx)
		}
	default:
		a.walk(v, ctx)
	}
}

// nested visits v, a collection of r's collections, one per fork of the
// reader root o.
func (a *aligner) nested(r, o *syntax.CallGraphStage, v syntax.Exp, ctx []alignCtx) {
	switch x := v.(type) {
	case *syntax.ArrayExp:
		for i, el := range x.Value {
			a.collection(r, el, append(slices.Clip(ctx), alignCtx{root: o, idx: ArrayIndex(i)}))
		}
	case *syntax.MapExp:
		for _, k := range slices.Sorted(maps.Keys(x.Value)) {
			a.collection(r, x.Value[k], append(slices.Clip(ctx), alignCtx{root: o, idx: MapKey(k), keys: x.Value}))
		}
	default:
		a.walk(v, ctx)
	}
}

// ref narrows, for each root of up the reader is not forked on, the reader
// roots a reference to up aligns it with: those of the enclosing splits at
// the reference's own index of it, whose keys, for a map, are the root's,
// or merging its forks where the reference leaves its index open.
func (a *aligner) ref(ref *syntax.RefExp, ctx []alignCtx) {
	for _, s := range a.roots {
		ix := ref.Forks[s.Call()]

		var found []string

		for _, c := range ctx {
			switch {
			case c.merge != nil:
				if c.merge.Fqid == s.Fqid && (ix == nil || ix.IndexSource() != nil) {
					found = append(found, c.root.Fqid)
				}
			case ix != nil && ix.IndexSource() == nil && indexEqual(c.idx, ix) && keysOf(s, c.keys):
				found = append(found, c.root.Fqid)
			}
		}

		if !a.seen {
			a.cand[s.Fqid] = found
		} else {
			a.cand[s.Fqid] = slices.DeleteFunc(a.cand[s.Fqid], func(x string) bool { return !slices.Contains(found, x) })
		}

		if len(a.cand[s.Fqid]) == 0 {
			a.unaligned[s.Fqid] = true
		}
	}

	a.seen = true
}

// keysOf reports whether keys, a map literal's, are root's own fork keys,
// or no map literal's: only then do the literal's ordinals, its keys in
// byte order, number root's forks alike.
func keysOf(root *syntax.CallGraphStage, keys map[string]syntax.Exp) bool {
	if keys == nil {
		return true
	}

	sp := root.Split()
	if sp == nil || !sp.Source.KnownLength() || sp.Source.CallMode() != syntax.ModeMapCall {
		return false
	}

	own := sp.Source.Keys()
	if len(own) != len(keys) {
		return false
	}

	for k := range keys {
		if _, ok := own[k]; !ok {
			return false
		}
	}

	return true
}
