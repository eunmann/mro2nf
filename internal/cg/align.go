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

// readPatterns collects, as the binding walk visits a reader's bindings,
// the ways its instances read each node's instances (Read.Patterns), one
// per shape of the references to the node, as an mrp job reads the forks
// of a node its specialized bindings name. A reference at the element of a
// split over a root of the reader reads, of each root of the node the
// reader is not forked on, the fork at the element's own index where every
// reference to the node under that root does (an alignment: Martian expands
// a split over a map call's outs into a literal of its forks, or a merge of
// them, in their order), and every fork otherwise; it does so only at that
// element where the split's elements do not all read the node (When).
type readPatterns struct {
	g      *Graph
	reader syntax.ForkRootList
	by     map[string]*aligner
}

func (g *Graph) readPatternsOf(st *syntax.CallGraphStage) *readPatterns {
	return &readPatterns{g: g, reader: st.ForkRoots(), by: map[string]*aligner{}}
}

func (p *readPatterns) ref(ref *syntax.RefExp, ctx []splitCtx) {
	a, ok := p.by[ref.Id]
	if !ok {
		a = p.aligner(ref.Id)
		p.by[ref.Id] = a
	}

	if a != nil {
		a.ref(ref, ctx)
	}
}

// aligner is up's, nil where up is no stage, so that the reader reads
// every instance of it under the roots they share. One of a node forked on
// no root the reader is not aligns nothing, and limits a read to the
// elements that reference the node all the same: Martian specializes a
// binding to each fork's element of a literal (syntax SplitExp.BindingPath),
// so a fork whose element names no node reads none of it.
func (p *readPatterns) aligner(up string) *aligner {
	u, err := p.g.Stage(up)
	if err != nil {
		return nil
	}

	a := &aligner{consistent: map[[2]string]bool{}}

	for _, r := range u.ForkRoots() {
		if !slices.ContainsFunc(p.reader, func(x *syntax.CallGraphStage) bool { return x.Fqid == r.Fqid }) {
			a.roots = append(a.roots, r)
		}
	}

	return a
}

// of are the read patterns of node, nil where the reader reads every
// instance of it under the roots they share.
func (p *readPatterns) of(node string) []ReadPattern {
	if a := p.by[node]; a != nil {
		return a.patterns()
	}

	return nil
}

// occurrence is one reference to up and the splits enclosing it.
type occurrence struct {
	ref *syntax.RefExp
	ctx []splitCtx
}

// aligner collects the references to up in a reader's bindings, and of each
// root of up the reader is not forked on, roots, and each reader root,
// whether every reference under a split over that reader root is at its
// element's own index of up's root (consistent).
type aligner struct {
	roots      syntax.ForkRootList
	occ        []occurrence
	consistent map[[2]string]bool
}

// ref records a reference to up, and of each pair of up's root and an
// enclosing split's reader root whether the reference is at the element's
// own index of up's root.
func (a *aligner) ref(ref *syntax.RefExp, ctx []splitCtx) {
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
func atOwnIndex(ref *syntax.RefExp, s *syntax.CallGraphStage, c splitCtx) bool {
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
			if !used[i] && c.merge == nil && c.n > 1 {
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
// every element of its split and a pattern another covers, so the patterns
// of a literal Martian expanded from an entry collection are the same at
// every width.
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

	// No two patterns left are equal, so none covers another but a wider one.
	var out []readPattern

	for _, q := range ps {
		wq := q.exported()
		if !slices.ContainsFunc(ps, func(p readPattern) bool {
			wp := p.exported()

			return !samePattern(wp, wq) && wp.covers(wq)
		}) {
			out = append(out, q)
		}
	}

	return out
}

// covers reports whether p reads, of every reader instance q reads at, at
// least the instances q does: p aligns no root q does not, at the same
// reader root, and limits no root to elements q is not limited within.
func (p ReadPattern) covers(q ReadPattern) bool {
	for u, r := range p.Align {
		if q.Align[u] != r {
			return false
		}
	}

	for r, ords := range p.When {
		qs, ok := q.When[r]
		if !ok || slices.ContainsFunc(qs, func(o int) bool { return !slices.Contains(ords, o) }) {
			return false
		}
	}

	return true
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
