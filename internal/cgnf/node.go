package cgnf

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/eunmann/mro2nf/internal/cg"
)

// node renders node i: its instances, enumerated root by root from the fork
// elements and records its roots' feeders write, each held until its
// preflights are done; its gates, which the driver evaluates so a disabled
// instance runs no task; the outs its enabled instances stage, field by
// field; its process call; and its outputs, a disabled instance standing in
// with none.
func (p *project) node(g *gen, i int, n cg.TopoNode) error {
	g.linef("// %s", n.Fqid)
	p.instances(g, i, n)
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
	in := p.input(i, n, meta)

	if n.Split {
		p.splitTriad(g, i, proc, in)
		proc += "_JN"
	} else {
		fmt.Fprintf(&g.incs, "include { STAGE as %s } from './stage.nf'\n", proc)
		g.linef("%s(%s)", proc, in)
	}

	if p.awaited(n.Fqid) {
		g.linef("d%d = %s.out.outs.toList().map { _xs -> true }", i, proc)
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

// input is the channel of node i's enabled instances as its process takes
// them, each with the entry files it reads: those PLAN lists for its fork
// of the node's entry-fed roots, and their indices, which mre args maps
// back to the run's values.
func (p *project) input(i int, n cg.TopoNode, meta string) string {
	if !p.readers[n.Fqid] {
		return fmt.Sprintf("u%d.combine(call).map { k, fs, es, d, us, c -> [%s, k, fs, es + us, [], []] }", i, meta)
	}

	var at []int

	for x, r := range n.Roots {
		if p.roots[r].Producer == "" {
			at = append(at, x)
		}
	}

	return fmt.Sprintf("u%d.combine(call).combine(entry).map { k, fs, es, d, us, c, ef -> def ids = ef.files[%s + ' ' + (%s.join('.') ?: '-')] ?: []; [%s, k, fs, es + us, ids, ids.collect { x -> ef.leaves[x] }] }",
		i, gstr(n.Fqid), pick("k", at), meta)
}

// instances enumerates node i's instances, i<i>: root by root, each with
// the partner elements it zips, held until its preflights are done.
func (p *project) instances(g *gen, i int, n cg.TopoNode) {
	zr := zipRoots(n)
	if len(n.Zips) == 0 {
		g.linef("i%d = channel.of([[], [], []])", i)
	} else {
		g.linef("i%d = channel.of([[], [], [], []])", i)
	}

	for _, root := range n.Roots {
		p.forkOn(g, i, n, root, zr)
	}

	for _, z := range n.Zips {
		p.zip(g, i, z, n, zr)
	}

	if len(n.Zips) > 0 {
		g.linef("i%d = i%d.map { k, fs, es, _tk -> [k, fs, es] }", i, i)
	}

	for _, pf := range n.Preflights {
		g.linef("i%d = i%d.combine(d%d).map { k, fs, es, _d -> [k, fs, es] }", i, i, p.index[pf])
	}
}

// zipRoots are node n's roots, in its order, that one of its zips splits
// over: an instance carries the index token of its fork of each.
func zipRoots(n cg.TopoNode) []string {
	var out []string

	for _, r := range n.Roots {
		if slices.ContainsFunc(n.Zips, func(z cg.Zip) bool { return z.Root == r }) {
			out = append(out, r)
		}
	}

	return out
}

// forkOn extends node i's instances by one of its roots: each instance
// joins every fork of root under the instance's parts for root's outer
// roots. An entry-fed root's forks are PLAN's records, passed as values; a
// stage-fed root's are the element dirs its producer posted. A node with
// zips carries the index token of its fork of each root in zr.
func (p *project) forkOn(g *gen, i int, n cg.TopoNode, root string, zr []string) {
	r := p.roots[root]

	outer := make([]int, len(r.Outer))
	for o, ro := range r.Outer {
		outer[o] = slices.Index(n.Roots, ro)
	}

	src, add := "f"+strconv.Itoa(slices.Index(p.entry, root)), "k + [own], fs + [x], es"
	if r.Producer != "" {
		src, add = p.elements(r), "k + [own], fs, es + [x]"
	}

	tup, zipped := "k, fs, es", false
	if len(n.Zips) > 0 {
		zipped = slices.Contains(zr, root)
		tup = "k, fs, es, tk"
		add += ", tk"

		if zipped {
			add += " + [t]"
		}
	}

	g.linef("i%d = i%d.map { %s -> [%s, %s] }.combine(%s, by: 0).map { _o, %s, own, x, %s -> [%s] }",
		i, i, tup, pick("k", outer), tup, src, tup, param("t", zipped), add)
}

// zip adds to each instance of node i, whose fork of root z.Root has the
// index token tk[j], z.Node's partner element at that token, posted by the
// instance of z.Node agreeing with it on z.Node's roots, or, where it has
// none, its absent record, which reads null or a missing element as mrp
// reads the output's element there.
func (p *project) zip(g *gen, i int, z cg.Zip, n cg.TopoNode, zr []string) {
	q := p.topo.Nodes[p.index[z.Node]]

	at := make([]int, len(q.Roots))
	for x, r := range q.Roots {
		at[x] = slices.Index(n.Roots, r)
	}

	g.linef("i%d = i%d.map { k, fs, es, tk -> [%s, k, fs, es, tk] }.combine(o%d_forks.map { kq, ds -> [kq, ds.findAll { e -> e.parent.name == %s }.collectEntries { e -> [(e.name): e] }] }, by: 0).map { _q, k, fs, es, tk, m -> [k, fs, es + ([m[tk[%d]] ?: m[%s]] - [null]), tk] }",
		i, i, pick("k", at), p.index[z.Node], gstr(cg.PartnerDir(z.Root, z.Node, z.Output)), slices.Index(zr, z.Root), gstr(cg.AbsentName))
}

// elements is the channel of root's fork element dirs its producer posted,
// each [outer key, own ordinal, dir, index token]: an outer root the
// producer is forked on takes the producer's ordinal for it, any other the
// ordinal the dir's name carries before its own. A map key's token follows
// the ordinals after an @; an array fork's is its ordinal.
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

	ix := "t[-1].toString()"
	if r.Keyed {
		ix = "e.name.tokenize('@')[1]"
	}

	return fmt.Sprintf("o%d_forks.flatMap { %s, ds -> ds.findAll { e -> e.parent.name == %s }.collect { e -> def t = e.name.tokenize('@')[0].tokenize('.').collect { x -> x as int }; [[%s], t[-1], e, %s] } }",
		p.index[r.Producer], param("kp", usesKey), gstr(r.Fqid), strings.Join(keys, ", "), ix)
}

// gate decides each instance of node i enabled or disabled, b<i>.run or
// b<i>.skip, from PLAN's record of the node, which each instance carries
// on as its d, and the gate values each stage whose outs gate it posted at
// the fork they share, keyed by the instance's ordinals of the gate's own
// roots.
func (p *project) gate(g *gen, i int, n cg.TopoNode) {
	fq := gstr(n.Fqid)
	g.linef("g%d = i%d.combine(nodes).map { k, fs, es, ns -> [k, fs, es, ns[%s], ns[%s][1] == '1' || ns[%s][2].tokenize(',').contains(k.join('.'))] }",
		i, i, fq, fq, fq)

	for _, gp := range n.Gates {
		prod := p.topo.Nodes[p.index[gp.Node]]

		at := make([]int, len(prod.Roots))
		for x, r := range prod.Roots {
			at[x] = slices.Index(n.Roots, r)
		}

		ext := make([]int, len(gp.Roots))
		for x, r := range gp.Roots {
			ext[x] = slices.Index(n.Roots, r)
		}

		g.linef("g%d = g%d.map { k, fs, es, d, off -> [%s, k, fs, es, d, off] }.combine(o%d_gates.map { kp, gs -> [kp, gs.tokenize(' ')] }, by: 0).map { _c, k, fs, es, d, off, gs -> [k, fs, es, d, off || gs.contains(%s + %s.join('.'))] }",
			i, i, pick("k", at), p.index[gp.Node], gstr(cg.GateToken(n.Fqid, gp, "")), pick("k", ext))
	}

	g.linef("b%d = g%d.branch { v ->", i, i)
	g.linef("  skip: v[4]")
	g.linef("  run: true")
	g.linef("}")
}

// read adds to each enabled instance of node i the outs bundles it reads of
// node r.Node: the instance agreeing on every root they share, or, when
// the upstream is forked on roots node i is not, every such instance, a
// gather ordered by the upstream's key, not by completion.
func (p *project) read(g *gen, i int, n cg.TopoNode, r cg.Read) {
	u := p.index[r.Node]
	up := p.topo.Nodes[u]

	var mine, theirs []int

	var extra []string

	for x, root := range up.Roots {
		if at := slices.Index(n.Roots, root); at >= 0 {
			mine, theirs = append(mine, at), append(theirs, x)
		} else {
			extra = append(extra, root)
		}
	}

	sel := "ds"
	if r.Fields != nil {
		sel = "ds.findAll { e -> e.name in " + glist(r.Fields) + " }"
	}

	src := fmt.Sprintf("o%d_outs.map { %s, ds -> [%s, %s] }", u, param("ku", len(theirs) > 0), pick("ku", theirs), sel)

	switch cnt, sized := p.forkCounts(n, up, extra); {
	case sized:
		p.sizedGather(g, i, n, up, sel, cnt)
		src = fmt.Sprintf("h%d_%d", i, u)
	case len(extra) > 0:
		g.linef("h%d_%d = b%d.run.map { %s -> [%s, null] }.unique().mix(o%d_outs.map { ku, ds -> [%s, %s.collect { e -> [ku, e] }] }).groupTuple().map { c, dss -> [c, ordered(dss.findAll { x -> x != null }.collectMany { x -> x })] }",
			i, u, i, param("v", len(mine) > 0), pick("v[0]", mine), u, pick("ku", theirs), sel)
		src = fmt.Sprintf("h%d_%d", i, u)
	}

	g.linef("u%d = u%d.map { k, fs, es, d, us -> [%s, k, fs, es, d, us] }.combine(%s, by: 0).map { _c, k, fs, es, d, us, ds -> [k, fs, es, d, us + ds] }",
		i, i, pick("k", mine), src)
}

// forkCounts is the channel of the fork counts of root extra[0] under each
// fork of its outer roots, [outer key, count], none left out, for a gather
// over it by a node i that is forked, each of whose instances then starts
// once its own share of the gather is in, not the whole of it. It is false
// for another gather: over several roots, or over one whose outer roots i
// does not share or whose producer's post enumerates them itself, whose
// counts no one item carries; such a gather waits for all of its upstream,
// as one consumed by an unforked node does by nature.
func (p *project) forkCounts(n, up cg.TopoNode, extra []string) (string, bool) {
	if len(extra) != 1 || len(n.Roots) == 0 || len(up.Roots) == 1 {
		return "", false
	}

	r := p.roots[extra[0]]
	for _, o := range r.Outer {
		if !slices.Contains(n.Roots, o) {
			return "", false
		}
	}

	if r.Producer == "" {
		return fmt.Sprintf("PLAN.out.n%d.flatMap { s -> s.readLines().collect { l -> def f = l.tokenize(' '); [f.size() > 1 ? f[1].tokenize('.').collect { t -> t as int } : [], f[0] as int] } }",
			slices.Index(p.entry, r.Fqid)), true
	}

	prod := p.topo.Nodes[p.index[r.Producer]]

	at := make([]int, len(r.Outer))
	for x, o := range r.Outer {
		if at[x] = slices.Index(prod.Roots, o); at[x] < 0 {
			return "", false
		}
	}

	return fmt.Sprintf("o%d_forks.map { %s, ds -> [%s, ds.count { e -> e.parent.name == %s }] }",
		p.index[r.Producer], param("kp", len(at) > 0), pick("kp", at), gstr(r.Fqid)), true
}

// sizedGather gathers, for each instance of node i, the outs of every
// instance of up under it, by groupKey with their count, which cnt gives
// for the outer fork of the gathered root the instance's key fixes; an
// instance under which the root has no fork gets an empty gather at once.
func (p *project) sizedGather(g *gen, i int, n, up cg.TopoNode, sel, cnt string) {
	extra := slices.IndexFunc(up.Roots, func(r string) bool { return !slices.Contains(n.Roots, r) })
	outer := p.roots[up.Roots[extra]].Outer

	var mine, theirs, outerMine, outerTheirs []int

	for x, root := range up.Roots {
		if at := slices.Index(n.Roots, root); at >= 0 {
			mine, theirs = append(mine, at), append(theirs, x)
		}

		if slices.Contains(outer, root) {
			outerMine, outerTheirs = append(outerMine, slices.Index(n.Roots, root)), append(outerTheirs, x)
		}
	}

	u := p.index[up.Fqid]
	g.linef("h%d_%d = o%d_outs.map { ku, ds -> [%s, %s, %s.collect { e -> [ku, e] }] }.combine(%s, by: 0).map { _o, c, xs, m -> [groupKey(c, m), xs] }.groupTuple().map { gk, dss -> [gk.getGroupTarget(), ordered(dss.collectMany { x -> x })] }.mix(b%d.run.map { v -> [%s, %s] }.unique().combine(%s, by: 0).filter { v -> v[2] == 0 }.map { v -> [v[1], []] })",
		i, u, u, pick("ku", outerTheirs), pick("ku", theirs), sel, cnt, i, pick("v[0]", outerMine), pick("v[0]", mine), cnt)
}

// splitTriad runs a split node: its split, a main per chunk the split
// returned, and its join over every chunk's outs once all are in, by chunk
// name, not by completion. Each chunk and the join are scheduled with the
// resources their jobs get, from the split's resources table.
func (p *project) splitTriad(g *gen, i int, proc, in string) {
	fmt.Fprintf(&g.incs, "include { STAGE_SPLIT as %[1]s_SP ; STAGE_MAIN as %[1]s_MN ; STAGE_JOIN as %[1]s_JN } from './stage.nf'\n", proc)
	g.linef("%s_SP(%s)", proc, in)
	g.linef("s%d = %s_SP.out.split", i, proc)
	g.linef("%s_MN(s%d.flatMap { m, k, a, cs, _j, res -> def r = resources(res); cs.collect { c -> [m + r[c.name] + [chunks: cs.size()], k, a, c.name, c] } })", proc, i)
	g.linef("c%d = %s_MN.out.chunk.map { m, k, o -> [groupKey(k, m.chunks), o] }.groupTuple().map { gk, os -> [gk.getGroupTarget(), os.sort(false) { a, b -> a.name <=> b.name }] }.mix(s%d.filter { v -> v[3].isEmpty() }.map { v -> [v[1], []] })",
		i, proc, i)
	g.linef("%s_JN(s%d.map { m, k, a, cs, j, res -> [k, m + resources(res).join, a, cs, j] }.join(c%d).map { k, m, a, cs, j, os -> [m, k, a, cs, j, os] })", proc, i, i)
}
