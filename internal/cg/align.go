package cg

import (
	"maps"
	"slices"
	"sort"

	"github.com/martian-lang/martian/martian/syntax"
)

// ReadPattern is one way an instance of a node reads instances of a node
// it reads (Read): every instance of it that agrees with the reader on the
// roots they share, and on each root in Align is at the reader's ordinal of
// the root Align maps it to; any ordinal of the node's other roots. When,
// if set, limits the pattern to the reader's instances at the listed
// ordinals of each root it names.
type ReadPattern struct {
	Align map[string]string `json:"align,omitempty"`
	When  map[string][]int  `json:"when,omitempty"`
}

// readPatterns are the ways st's instances read up's instances, one per
// shape of the references to up in st's bindings, as an mrp job reads the
// forks of up its specialized bindings name. A reference at the element of
// a split over a root of st reads, of each root of up st is not forked on,
// the fork at the element's own index where every reference to up under
// that root does (an alignment: Martian expands a split over a map call's
// outs into a literal of its forks, or a merge of them, in their order),
// and every fork otherwise; it does so only at that element where the
// split's elements do not all read up (When). It is nil where st reads every
// instance of up under the roots they share.
func (g *Graph) readPatterns(st *syntax.CallGraphStage, up string) []ReadPattern {
	u, err := g.Stage(up)
	if err != nil {
		return nil
	}

	a := aligner{g: g, reader: st.ForkRoots(), up: up, consistent: map[[2]string]bool{}}

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

	return a.patterns()
}

// alignCtx is an enclosing split over the reader root root, at its element
// at idx, the ordinal ord of the n elements of the split's literal, or at
// the merge of root merge's forks, whose elements no ordinal names.
type alignCtx struct {
	root  *syntax.CallGraphStage
	idx   syntax.CollectionIndex
	keys  map[string]syntax.Exp
	ord   int
	n     int
	merge *syntax.CallGraphStage
}

// occurrence is one reference to up and the splits enclosing it.
type occurrence struct {
	ref *syntax.RefExp
	ctx []alignCtx
}

// aligner collects the references to up in a reader's bindings, and of each
// root of up the reader is not forked on and each reader root, whether
// every reference under a split over that reader root is at its element's
// own index of up's root (consistent).
type aligner struct {
	g          *Graph
	reader     syntax.ForkRootList
	up         string
	roots      syntax.ForkRootList
	occ        []occurrence
	consistent map[[2]string]bool
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
		if a.g.mergeGathersNone(x, a.reader) {
			return
		}

		a.walk(x.Value, ctx)

		if x.ForkNode != nil {
			a.walk(x.ForkNode, ctx)
		}

		a.countReads(x, ctx)
	case *syntax.DisabledExp:
		a.walk(x.Disabled, ctx)
		a.walk(x.Value, ctx)
	}
}

// countReads visits what counting the forks the merge x gathers reads
// (Graph.mergeCounted, Graph.countReads), each at the indices of the outer
// roots the merge's constraint fixes, where the merge resolves it
// (mergeIndices).
func (a *aligner) countReads(x *syntax.MergeExp, ctx []alignCtx) {
	root, ok := a.g.mergeCounted(x, a.reader)
	if !ok {
		return
	}

	constraint := mergeConstraint(x)

	for _, r := range a.g.countReads(root) {
		if r.Id != a.up {
			continue
		}

		at := *r
		at.Forks = maps.Clone(r.Forks)

		if constraint != nil {
			for c, ix := range constraint.Forks {
				if ix != nil && ix.IndexSource() == nil {
					if was, ok := at.Forks[c]; !ok || was == nil || was.IndexSource() != nil {
						at.Forks[c] = ix
					}
				}
			}
		}

		a.ref(&at, ctx)
	}
}

// collection visits v, the collection a split over the reader root r
// forks, element by element.
func (a *aligner) collection(r *syntax.CallGraphStage, v syntax.Exp, ctx []alignCtx) {
	switch x := v.(type) {
	case *syntax.ArrayExp:
		for i, el := range x.Value {
			a.walk(el, append(slices.Clip(ctx), alignCtx{root: r, idx: ArrayIndex(i), ord: i, n: len(x.Value)}))
		}
	case *syntax.MapExp:
		keys := slices.Sorted(maps.Keys(x.Value))
		for i, k := range keys {
			a.walk(x.Value[k], append(slices.Clip(ctx), alignCtx{root: r, idx: MapKey(k), keys: x.Value, ord: i, n: len(keys)}))
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
			a.collection(r, el, append(slices.Clip(ctx), alignCtx{root: o, idx: ArrayIndex(i), ord: i, n: len(x.Value)}))
		}
	case *syntax.MapExp:
		keys := slices.Sorted(maps.Keys(x.Value))
		for i, k := range keys {
			a.collection(r, x.Value[k], append(slices.Clip(ctx), alignCtx{root: o, idx: MapKey(k), keys: x.Value, ord: i, n: len(keys)}))
		}
	default:
		a.walk(v, ctx)
	}
}

// ref records a reference to up, and of each pair of up's root and an
// enclosing split's reader root whether the reference is at the element's
// own index of up's root.
func (a *aligner) ref(ref *syntax.RefExp, ctx []alignCtx) {
	a.occ = append(a.occ, occurrence{ref: ref, ctx: ctx})

	for _, s := range a.roots {
		for _, c := range ctx {
			k := [2]string{s.Fqid, c.root.Fqid}

			was, seen := a.consistent[k]
			a.consistent[k] = (!seen || was) && atOwnIndex(ref, s, c)
		}
	}
}

// atOwnIndex reports whether ref reads root s's fork at the index of its
// element of the enclosing split c: the element's own index, its key being
// one of s's own keys, or, in a merge of s's forks, the fork it merges.
func atOwnIndex(ref *syntax.RefExp, s *syntax.CallGraphStage, c alignCtx) bool {
	ix := ref.Forks[s.Call()]
	unresolved := ix == nil || ix.IndexSource() != nil

	if c.merge != nil {
		return c.merge.Fqid == s.Fqid && unresolved
	}

	return !unresolved && indexEqual(c.idx, ix) && keysOf(s, c.keys)
}

// patterns are the read patterns of the occurrences: each aligns a root of
// up with the innermost enclosing split over a reader root it is
// consistent with, and is limited to the element of each other enclosing
// split it is at, the patterns that differ only in the elements of one
// root merged, and a limit to every element of a split dropped.
func (a *aligner) patterns() []ReadPattern {
	var ps []readPattern

	for _, o := range a.occ {
		p := readPattern{align: map[string]string{}, when: map[string][]int{}, n: map[string]int{}}
		used := map[int]bool{}

		for _, s := range a.roots {
			for i, v := range slices.Backward(o.ctx) {
				c := v
				if a.consistent[[2]string{s.Fqid, c.root.Fqid}] && atOwnIndex(o.ref, s, c) {
					p.align[s.Fqid] = c.root.Fqid
					used[i] = true

					break
				}
			}
		}

		for i, c := range o.ctx {
			if !used[i] && c.merge == nil {
				p.when[c.root.Fqid] = []int{c.ord}
				p.n[c.root.Fqid] = c.n
			}
		}

		ps = append(ps, p)
	}

	ps = mergePatterns(ps)

	if len(ps) == 0 || (len(ps) == 1 && len(ps[0].align) == 0 && len(ps[0].when) == 0) {
		return nil
	}

	out := make([]ReadPattern, len(ps))
	for i, p := range ps {
		out[i] = p.exported()
	}

	return out
}

// readPattern is a ReadPattern being merged, with the element count of the
// split each limit is over.
type readPattern struct {
	align map[string]string
	when  map[string][]int
	n     map[string]int
}

func (p readPattern) exported() ReadPattern {
	out := ReadPattern{}
	if len(p.align) > 0 {
		out.Align = p.align
	}

	if len(p.when) > 0 {
		out.When = p.when
	}

	return out
}

// mergePatterns merges, until none do, the patterns that differ only in the
// elements of one root they are limited to, and drops a limit that comes to
// every element of its split.
func mergePatterns(ps []readPattern) []readPattern {
	for merged := true; merged; {
		merged = false

		for i := 0; i < len(ps) && !merged; i++ {
			for j := i + 1; j < len(ps) && !merged; j++ {
				if root, ok := mergeable(ps[i], ps[j]); ok {
					ps[i] = unionPatterns(ps[i], ps[j], root)
					ps = slices.Delete(ps, j, j+1)
					merged = true
				}
			}
		}
	}

	return ps
}

// mergeable reports whether p and q differ only in the elements of one
// root, and which, or not at all ("").
func mergeable(p, q readPattern) (string, bool) {
	if !maps.Equal(p.align, q.align) || !slices.Equal(slices.Sorted(maps.Keys(p.when)), slices.Sorted(maps.Keys(q.when))) {
		return "", false
	}

	var differ []string

	for r, ords := range p.when {
		if !slices.Equal(ords, q.when[r]) {
			differ = append(differ, r)
		}
	}

	switch len(differ) {
	case 0:
		return "", true
	case 1:
		return differ[0], true
	default:
		return "", false
	}
}

// unionPatterns is p limited to the elements of root either of p and q is,
// its limit to root dropped where they come to all of the split's.
func unionPatterns(p, q readPattern, root string) readPattern {
	if root == "" {
		return p
	}

	ords := append(slices.Clone(p.when[root]), q.when[root]...)
	sort.Ints(ords)
	ords = slices.Compact(ords)

	when := maps.Clone(p.when)
	if len(ords) == p.n[root] {
		delete(when, root)
	} else {
		when[root] = ords
	}

	return readPattern{align: p.align, when: when, n: p.n}
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
