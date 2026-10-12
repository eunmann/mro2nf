package cg

import (
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/martian-lang/martian/martian/syntax"
)

// agreeItem is a fork with the ordinals of the roots its key names; either
// may be empty.
type agreeItem struct {
	ords map[string]int
	fork Fork
}

// agreeIndex finds the items that agree with a fork, as agrees and
// ordsAgree decide it: equal at every root both name. Items are grouped by
// the roots they name, and a group is keyed by its values at the roots a
// query shares with it, so a lookup costs the matches it returns, not a scan
// of every item.
type agreeIndex struct {
	items  []agreeItem
	groups []*agreeGroup
}

type agreeGroup struct {
	ordRoots, partRoots []string
	members             []int
	keyed               map[string]map[string][]int
}

func newAgreeIndex(items []agreeItem) *agreeIndex {
	x := &agreeIndex{items: items}
	bySig := map[string]*agreeGroup{}

	for i, it := range items {
		ords := slices.Sorted(maps.Keys(it.ords))

		parts := make([]string, len(it.fork))
		for j, p := range it.fork {
			parts[j] = p.Root.Fqid
		}

		slices.Sort(parts)

		sig := strings.Join(ords, ",") + "|" + strings.Join(parts, ",")

		g, ok := bySig[sig]
		if !ok {
			g = &agreeGroup{ordRoots: ords, partRoots: parts, keyed: map[string]map[string][]int{}}
			bySig[sig] = g
			x.groups = append(x.groups, g)
		}

		g.members = append(g.members, i)
	}

	return x
}

// agreeing are the positions, in order, of the items agreeing with ords and
// fork.
func (x *agreeIndex) agreeing(ords map[string]int, fork Fork) []int {
	at := partsByRoot(fork)

	if len(x.groups) == 1 {
		return x.groups[0].matching(x.items, ords, at)
	}

	found := make([][]int, len(x.groups))
	n := 0

	for i, g := range x.groups {
		found[i] = g.matching(x.items, ords, at)
		n += len(found[i])
	}

	out := make([]int, 0, n)
	for _, f := range found {
		out = append(out, f...)
	}

	slices.Sort(out)

	return out
}

// matching are g's members agreeing with ords and the parts at, keying g by
// the roots they share on first use.
func (g *agreeGroup) matching(items []agreeItem, ords map[string]int, at map[string]syntax.CollectionIndex) []int {
	so := sharedRoots(g.ordRoots, func(r string) bool {
		_, ok := ords[r]

		return ok
	})
	sp := sharedRoots(g.partRoots, func(r string) bool {
		_, ok := at[r]

		return ok
	})
	sig := strings.Join(so, ",") + "|" + strings.Join(sp, ",")

	byKey, ok := g.keyed[sig]
	if !ok {
		byKey = map[string][]int{}

		for _, m := range g.members {
			k := agreeKey(so, sp, items[m].ords, partsByRoot(items[m].fork))
			byKey[k] = append(byKey[k], m)
		}

		g.keyed[sig] = byKey
	}

	return byKey[agreeKey(so, sp, ords, at)]
}

func sharedRoots(roots []string, has func(string) bool) []string {
	var out []string

	for _, r := range roots {
		if has(r) {
			out = append(out, r)
		}
	}

	return out
}

func partsByRoot(f Fork) map[string]syntax.CollectionIndex {
	out := make(map[string]syntax.CollectionIndex, len(f))
	for _, p := range f {
		out[p.Root.Fqid] = p.Index
	}

	return out
}

// agreeKey is the values of ords at ordRoots and of parts at partRoots, as
// indexEqual tells indices apart: by mode, then position or key.
func agreeKey(ordRoots, partRoots []string, ords map[string]int, parts map[string]syntax.CollectionIndex) string {
	var b strings.Builder

	for _, r := range ordRoots {
		b.WriteString(strconv.Itoa(ords[r]))
		b.WriteByte(0)
	}

	for _, r := range partRoots {
		ix := parts[r]
		if ix.Mode() == syntax.ModeMapCall {
			b.WriteString("k" + strconv.Quote(ix.MapKey()))
		} else {
			b.WriteString("a" + strconv.Itoa(ix.ArrayIndex()))
		}

		b.WriteByte(0)
	}

	return b.String()
}
