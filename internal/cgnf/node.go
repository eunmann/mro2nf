package cgnf

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/eunmann/mro2nf/internal/cg"
)

// node renders node i: its instances, enumerated root by root from the fork
// elements and records its roots' feeders write; its gates, which the
// driver evaluates so a disabled instance runs no task; the outs its
// enabled instances stage, field by field; its process call; and its
// outputs, a disabled instance standing in with none.
func (p *project) node(g *gen, i int, n cg.TopoNode) error {
	g.linef("// %s", n.Fqid)
	g.linef("i%d = channel.of([[], [], []])", i)

	for _, root := range n.Roots {
		p.forkOn(g, i, n, root)
	}

	p.gate(g, i, n)
	g.linef("u%d = b%d.run.map { k, fs, es, d, _off -> [k, fs, es, d, []] }", i, i)

	for _, r := range n.Reads {
		p.read(g, i, n, r)
	}

	meta, err := p.nodeMeta(n)
	if err != nil {
		return err
	}

	proc := alias(i, n.Fqid)
	in := fmt.Sprintf("u%d.map { k, fs, es, d, us -> [%s, k, fs, es + us] }", i, meta)

	if n.Split {
		p.splitTriad(g, i, proc, in)
		proc += "_JN"
	} else {
		fmt.Fprintf(&g.incs, "include { STAGE as %s } from './stage.nf'\n", proc)
		g.linef("%s(%s)", proc, in)
	}

	outs, forks, gates := p.consumed(n.Fqid)
	for _, o := range []struct {
		used       bool
		name, none string
	}{{outs, "outs", "[]"}, {forks, "forks", "[]"}, {gates, "gates", "''"}} {
		if o.used {
			g.linef("o%d_%s = %s.out.%s.mix(b%d.skip.map { v -> [v[0], %s] })", i, o.name, proc, o.name, i, o.none)
		}
	}

	return nil
}

// forkOn extends node i's instances by one of its roots: each instance
// joins every fork of root under the instance's parts for root's outer
// roots. An entry-fed root's forks are PLAN's records, passed as values; a
// stage-fed root's are the element dirs its producer posted.
func (p *project) forkOn(g *gen, i int, n cg.TopoNode, root string) {
	r := p.roots[root]

	outer := make([]int, len(r.Outer))
	for o, ro := range r.Outer {
		outer[o] = slices.Index(n.Roots, ro)
	}

	src, add := "f"+strconv.Itoa(slices.Index(p.entry, root)), "[k + [own], fs + [x], es]"
	if r.Producer != "" {
		src, add = p.elements(r), "[k + [own], fs, es + [x]]"
	}

	g.linef("i%d = i%d.map { k, fs, es -> [%s, k, fs, es] }.combine(%s, by: 0).map { _o, k, fs, es, own, x -> %s }",
		i, i, pick("k", outer), src, add)
}

// elements is the channel of root's fork element dirs its producer posted,
// each [outer key, own ordinal, dir]: an outer root the producer is forked
// on takes the producer's ordinal for it, any other the ordinal the dir's
// name carries before its own.
func (p *project) elements(r cg.TopoRoot) string {
	prod := p.topo.Nodes[p.index[r.Producer]]

	keys := make([]string, 0, len(r.Outer))
	named, usesKey := 0, false

	for _, o := range r.Outer {
		if at := slices.Index(prod.Roots, o); at >= 0 {
			keys = append(keys, "kp["+strconv.Itoa(at)+"]")
			usesKey = true

			continue
		}

		keys = append(keys, "t["+strconv.Itoa(named)+"]")
		named++
	}

	return fmt.Sprintf("o%d_forks.flatMap { %s, ds -> ds.findAll { e -> e.parent.name == %s }.collect { e -> def t = e.name.tokenize('.').collect { x -> x as int }; [[%s], t[-1], e] } }",
		p.index[r.Producer], param("kp", usesKey), gstr(r.Fqid), strings.Join(keys, ", "))
}

// gate decides each instance of node i enabled or disabled, b<i>.run or
// b<i>.skip, from PLAN's record of the node and the gate values each stage
// whose outs gate it posted at the fork they share.
func (p *project) gate(g *gen, i int, n cg.TopoNode) {
	fq := gstr(n.Fqid)
	g.linef("g%d = i%d.combine(nodes).map { k, fs, es, ns -> [k, fs, es, ns[%s][0], ns[%s][1] == '1' || ns[%s][2].tokenize(',').contains(k.join('.'))] }",
		i, i, fq, fq, fq)

	for _, gp := range n.Gates {
		prod := p.topo.Nodes[p.index[gp]]

		at := make([]int, len(prod.Roots))
		for x, r := range prod.Roots {
			at[x] = slices.Index(n.Roots, r)
		}

		g.linef("g%d = g%d.map { k, fs, es, d, off -> [%s, k, fs, es, d, off] }.combine(o%d_gates.map { kp, gs -> [kp, gs.tokenize(' ').contains(%s)] }, by: 0).map { _c, k, fs, es, d, off, by -> [k, fs, es, d, off || by] }",
			i, i, pick("k", at), p.index[gp], fq)
	}

	g.linef("b%d = g%d.branch { v ->", i, i)
	g.linef("  skip: v[4]")
	g.linef("  run: true")
	g.linef("}")
}

// read adds to each enabled instance of node i the outs bundles it reads of
// node r.Node: the instance agreeing on every root they share, or, when
// the upstream is forked on roots node i is not, every such instance, a
// gather seeded so an empty one still yields its consumer and ordered by
// the upstream's key, not by completion.
func (p *project) read(g *gen, i int, n cg.TopoNode, r cg.Read) {
	u := p.index[r.Node]
	up := p.topo.Nodes[u]

	var mine, theirs []int

	gather := false

	for x, root := range up.Roots {
		if at := slices.Index(n.Roots, root); at >= 0 {
			mine, theirs = append(mine, at), append(theirs, x)
		} else {
			gather = true
		}
	}

	sel := "ds"
	if r.Fields != nil {
		sel = "ds.findAll { e -> e.name in " + glist(r.Fields) + " }"
	}

	src := fmt.Sprintf("o%d_outs.map { %s, ds -> [%s, %s] }", u, param("ku", len(theirs) > 0), pick("ku", theirs), sel)

	if gather {
		g.linef("h%d_%d = b%d.run.map { %s -> [%s, null] }.unique().mix(o%d_outs.map { ku, ds -> [%s, %s.collect { e -> [ku, e] }] }).groupTuple().map { c, dss -> [c, ordered(dss.findAll { x -> x != null }.collectMany { x -> x })] }",
			i, u, i, param("v", len(mine) > 0), pick("v[0]", mine), u, pick("ku", theirs), sel)
		src = fmt.Sprintf("h%d_%d", i, u)
	}

	g.linef("u%d = u%d.map { k, fs, es, d, us -> [%s, k, fs, es, d, us] }.combine(%s, by: 0).map { _c, k, fs, es, d, us, ds -> [k, fs, es, d, us + ds] }",
		i, i, pick("k", mine), src)
}

// splitTriad runs a split node: its split, a main per chunk the split
// returned, and its join over every chunk's outs once all are in, by chunk
// name, not by completion.
func (p *project) splitTriad(g *gen, i int, proc, in string) {
	fmt.Fprintf(&g.incs, "include { STAGE_SPLIT as %[1]s_SP ; STAGE_MAIN as %[1]s_MN ; STAGE_JOIN as %[1]s_JN } from './stage.nf'\n", proc)
	g.linef("%s_SP(%s)", proc, in)
	g.linef("s%d = %s_SP.out.split", i, proc)
	g.linef("%s_MN(s%d.flatMap { m, k, a, cs, _j -> cs.collect { c -> [m + [chunks: cs.size()], k, a, c] } })", proc, i)
	g.linef("c%d = %s_MN.out.chunk.map { m, k, o -> [groupKey(k, m.chunks), o] }.groupTuple().map { gk, os -> [gk.getGroupTarget(), os.sort(false) { a, b -> a.name <=> b.name }] }.mix(s%d.filter { v -> v[3].isEmpty() }.map { v -> [v[1], []] })",
		i, proc, i)
	g.linef("%s_JN(s%d.map { m, k, a, cs, j -> [k, m, a, cs, j] }.join(c%d).map { k, m, a, cs, j, os -> [m, k, a, cs, j, os] })", proc, i, i)
}
