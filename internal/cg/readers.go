package cg

import (
	"iter"
	"maps"
	"slices"

	"github.com/martian-lang/martian/martian/syntax"
)

// postReaders is what a node's post writes for the nodes that read its
// outs, beyond its own spec: per root its outs fix the forks of, the root,
// the outer roots whose forks it enumerates and the outputs its fork
// elements zip; each partner of its outputs; each disable binding whose
// value it posts, with the roots that binding is forked on beyond the
// node's. A task's cache key holds it, so a re-transpile that changes it
// re-runs the node, whose cached post would write for the old readers.
type postReaders struct {
	Roots    []fedRoot `json:"roots"`
	Partners []rootZip `json:"partners"`
	Gates    []gateKey `json:"gates"`
}

// rootKey is a fork root: its split, in Martian's JSON form of a call
// graph, the collection type that form omits and an element is written
// with, and the roots its forks vary with.
type rootKey struct {
	Fqid  string           `json:"fqid"`
	Split *syntax.SplitExp `json:"split"`
	Type  string           `json:"type"`
	Outer []string         `json:"outer"`
}

type fedRoot struct {
	Root  rootKey   `json:"root"`
	Outer []rootKey `json:"outer"`
	Zips  []rootZip `json:"zips"`
}

// rootZip is a split over a root an element holds a member of: the member,
// or the partner dir, the split and its collection type.
type rootZip struct {
	Name  string           `json:"name"`
	Root  string           `json:"root"`
	Split *syntax.SplitExp `json:"split"`
	Type  string           `json:"type"`
}

type gateKey struct {
	Node    string     `json:"node"`
	Binding syntax.Exp `json:"binding"`
	Roots   []rootKey  `json:"roots"`
}

func (g *Graph) postReaders(st *syntax.CallGraphStage) (postReaders, error) {
	out := postReaders{Roots: []fedRoot{}, Partners: []rootZip{}, Gates: []gateKey{}}

	for _, root := range g.fedBy(st.Fqid) {
		srcs, err := g.zipSources(root)
		if err != nil {
			return postReaders{}, err
		}

		fr := fedRoot{Root: g.rootKey(root), Outer: g.rootKeys(g.outerRoots(root)), Zips: []rootZip{}}
		for _, s := range srcs {
			fr.Zips = append(fr.Zips, newRootZip(s.member, root, s.split))
		}

		out.Roots = append(out.Roots, fr)
	}

	ps, err := g.partners(st.Fqid)
	if err != nil {
		return postReaders{}, err
	}

	for _, p := range ps {
		out.Partners = append(out.Partners, newRootZip(p.dir(), p.root, p.split))
	}

	for gated, d := range g.gatedBy(st.Fqid) {
		extra, err := g.gateRoots(gated, d, st)
		if err != nil {
			return postReaders{}, err
		}

		out.Gates = append(out.Gates, gateKey{Node: gated.Fqid, Binding: d, Roots: g.rootKeys(extra)})
	}

	return out, nil
}

func (g *Graph) rootKey(root *syntax.CallGraphStage) rootKey {
	sp := root.Split()

	return rootKey{Fqid: root.Fqid, Split: sp, Type: splitType(sp), Outer: fqids(g.outerRoots(root))}
}

func (g *Graph) rootKeys(roots syntax.ForkRootList) []rootKey {
	out := make([]rootKey, len(roots))
	for i, r := range roots {
		out[i] = g.rootKey(r)
	}

	return out
}

func newRootZip(name string, root *syntax.CallGraphStage, sp *syntax.SplitExp) rootZip {
	return rootZip{Name: name, Root: root.Fqid, Split: sp, Type: splitType(sp)}
}

func splitType(sp *syntax.SplitExp) string {
	if sp == nil || sp.Type == nil {
		return ""
	}

	return sp.Type.String()
}

// fedBy are the roots whose forks node's outs fix, in fqid order.
func (g *Graph) fedBy(node string) []*syntax.CallGraphStage {
	var out []*syntax.CallGraphStage

	for _, root := range g.sortedRoots() {
		if p, ok := g.Producer(root); ok && p == node {
			out = append(out, root)
		}
	}

	return out
}

// gatedBy yields each disable binding, of the stages in fqid order, that
// reads node's outs alone: node's post posts its value.
func (g *Graph) gatedBy(node string) iter.Seq2[*syntax.CallGraphStage, syntax.Exp] {
	return func(yield func(*syntax.CallGraphStage, syntax.Exp) bool) {
		for _, fqid := range slices.Sorted(maps.Keys(g.stages)) {
			st := g.stages[fqid]

			for _, d := range st.Disabled() {
				if refs := refIDs(d); len(refs) == 1 && refs[0] == node && !yield(st, d) {
					return
				}
			}
		}
	}
}
