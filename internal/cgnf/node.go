package cgnf

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/eunmann/mro2nf/internal/cg"
)

// wiring is a run of workflow statements: its lines, the stage module
// aliases they call, and the channels they define and read, which tie it
// to the runs before and after it.
type wiring struct {
	lines, incs []string
	uses, defs  []string
}

func (w *wiring) linef(format string, args ...any) {
	w.lines = append(w.lines, fmt.Sprintf(format, args...))
}

// use is channel name, which w reads.
func (w *wiring) use(name string) string {
	if !slices.Contains(w.uses, name) {
		w.uses = append(w.uses, name)
	}

	return name
}

// def is channel name, which w defines.
func (w *wiring) def(name string) string {
	w.defs = append(w.defs, name)

	return name
}

// The channels a node defines for the nodes after it.
func outsOf(i int) string  { return "o" + strconv.Itoa(i) + "_outs" }
func forksOf(i int) string { return "o" + strconv.Itoa(i) + "_forks" }
func gatesOf(i int) string { return "o" + strconv.Itoa(i) + "_gates" }
func doneOf(i int) string  { return "d" + strconv.Itoa(i) }

// entryForks is the channel of PLAN's record of the forks of every
// entry-fed root, a map by root.
const entryForks = "entry_forks"

// entryRoot is the channel of root's own forks of entryForks.
func entryRoot(w *wiring, root string) string {
	return fmt.Sprintf("ENTRY_ROOT(%s, %s)", w.use(entryForks), gstr(root))
}

// node wires node i: its instances, enumerated root by root from the fork
// elements and records its roots' feeders write; its gates, which the
// driver evaluates once its preflights are done so a disabled instance
// runs no task; the outs its enabled instances stage, field by field; its
// process call; its outputs, each file one names by reference resolved
// against what its posting task staged, and a disabled instance standing
// in with none; and, of a preflight, PLAN's records held until it is done.
func (p *project) node(i int, n cg.TopoNode) (*wiring, error) {
	meta, err := p.nodeMeta(n)
	if err != nil {
		return nil, err
	}

	w := &wiring{}
	w.linef("// %s", n.Fqid)
	p.gate(w, i, n, p.instances(w, i, n), meta)

	run := fmt.Sprintf("b%d.run", i)
	for _, r := range n.Reads {
		w.linef("u%d = READ(%s, %s)", i, run, p.read(w, i, n, r))
		run = "u" + strconv.Itoa(i)
	}

	proc := alias(i, n.Fqid)
	in := p.input(w, n, run)

	sources := fmt.Sprintf("SOURCES(n%d)", i)
	if n.Split {
		splitTriad(w, i, proc, in)
		proc += "_JN"
		sources = fmt.Sprintf("JOIN_SOURCES(j%d)", i)
	} else {
		w.incs = append(w.incs, "STAGE as "+proc)
		w.linef("n%d = %s", i, in)
		w.linef("%s(n%d)", proc, i)
	}

	if holds := p.holdsAt(i); len(holds) > 0 {
		done := w.def(doneOf(i))
		w.linef("%s = DONE(%s.out.outs)", done, proc)

		for _, h := range holds {
			w.linef("%s = AFTER(%s, %s)", w.def(nodesAfter(h)), w.use(nodesAfter(h[:len(h)-1])), done)
		}
	}

	outs, forks, gates := p.consumed(n.Fqid)
	if outs || forks {
		w.linef("r%d = %s", i, sources)
	}

	for _, o := range []struct {
		used      bool
		name, out string
	}{{outs, outsOf(i), "outs"}, {forks, forksOf(i), "forks"}} {
		if o.used {
			w.linef("%s = POSTED(%s.out.%s, b%d.skip, r%d)", w.def(o.name), proc, o.out, i, i)
		}
	}

	if gates {
		w.linef("%s = GATES(%s.out.gates, b%d.skip)", w.def(gatesOf(i)), proc, i)
	}

	return w, nil
}

// input is the channel of node i's enabled instances, run, as its process
// takes them, each with the entry files it reads: those PLAN lists for its
// fork of the node's entry-fed roots.
func (p *project) input(w *wiring, n cg.TopoNode, run string) string {
	if !p.readers[n.Fqid] {
		return fmt.Sprintf("INPUT(%s)", run)
	}

	var at []int

	for x, r := range n.Roots {
		if p.roots[r].Producer == "" {
			at = append(at, x)
		}
	}

	return fmt.Sprintf("INPUT_ENTRY(%s, %s, %s)", run, w.use("entry"), ilist(at))
}

// instances is the expression of node i's instances: root by root, each
// with the partner elements it zips. Each step is assigned to i<i>, which
// it returns, or the seed channel when the node has no step.
func (p *project) instances(w *wiring, i int, n cg.TopoNode) string {
	inst := "channel.of([[], [], [], []])"
	step := func(expr string) {
		w.linef("i%d = %s", i, expr)
		inst = "i" + strconv.Itoa(i)
	}

	zr := zipRoots(n)
	for _, root := range n.Roots {
		step(p.forkOn(w, inst, n, root, zr))
	}

	for _, z := range n.Zips {
		step(p.zip(w, inst, z, n, zr))
	}

	return inst
}

// nodesAfter is the channel of PLAN's node records held until every
// preflight of pfs, node indices in ascending order, is done; nodes itself
// for none.
func nodesAfter(pfs []int) string {
	if len(pfs) == 0 {
		return "nodes"
	}

	ids := make([]string, len(pfs))
	for x, i := range pfs {
		ids[x] = strconv.Itoa(i)
	}

	return "nodes_after_" + strings.Join(ids, "_")
}

// preflightsOf are the node indices of n's preflights, ascending.
func (p *project) preflightsOf(n cg.TopoNode) []int {
	out := make([]int, len(n.Preflights))
	for x, pf := range n.Preflights {
		out[x] = p.index[pf]
	}

	slices.Sort(out)

	return out
}

// holdsAt are the prefixes of every node's preflights that end at
// preflight i, each held once, after the prefix before it, whatever the
// number of nodes that wait for it. Nextflow adds each edge of its DAG in
// time linear in the edges before it, so a hold per node would make the
// launch quadratic in the nodes.
func (p *project) holdsAt(i int) [][]int {
	var out [][]int

	for _, n := range p.topo.Nodes {
		pfs := p.preflightsOf(n)

		at := slices.Index(pfs, i)
		if at >= 0 && !slices.ContainsFunc(out, func(h []int) bool { return slices.Equal(h, pfs[:at+1]) }) {
			out = append(out, pfs[:at+1])
		}
	}

	return out
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

// positions are the positions in roots of each of xs, -1 for one it lacks.
func positions(roots, xs []string) []int {
	out := make([]int, len(xs))
	for x, r := range xs {
		out[x] = slices.Index(roots, r)
	}

	return out
}

// forkOn extends node n's instances, inst, by one of its roots. An
// entry-fed root's forks are PLAN's records, passed as values; a stage-fed
// root's are the element items its producer posted. A root in zr takes its
// index token.
func (p *project) forkOn(w *wiring, inst string, n cg.TopoNode, root string, zr []string) string {
	r := p.roots[root]

	entryFed := r.Producer == ""

	var src string
	if entryFed {
		src = entryRoot(w, root)
	} else {
		src = p.elements(w, r)
	}

	return fmt.Sprintf("FORK(%s, %s, %s, %t, %t)", inst, src, ilist(positions(n.Roots, r.Outer)), entryFed, slices.Contains(zr, root))
}

// zip adds to node n's instances, inst, z.Node's partner element at the
// index token of their fork of z.Root.
func (p *project) zip(w *wiring, inst string, z cg.Zip, n cg.TopoNode, zr []string) string {
	q := p.index[z.Node]

	return fmt.Sprintf("ZIP(%s, %s, %s, %s, %d, %s)", inst, w.use(forksOf(q)), ilist(positions(n.Roots, p.topo.Nodes[q].Roots)),
		gstr(cg.PartnerDir(z.Root, z.Node, z.Output)), slices.Index(zr, z.Root), gstr(cg.AbsentName))
}

// elements is the channel of root's fork element items its producer
// posted: an outer root the producer is forked on takes the producer's
// ordinal for it, any other the ordinal the item's name carries.
func (p *project) elements(w *wiring, r cg.TopoRoot) string {
	prod := p.index[r.Producer]

	return fmt.Sprintf("ELEMENTS(%s, %s, %s, %t)", w.use(forksOf(prod)), gstr(r.Fqid), ilist(positions(p.topo.Nodes[prod].Roots, r.Outer)), r.Keyed)
}

// gate decides each of node i's instances, inst, enabled or disabled,
// b<i>.run or b<i>.skip, from PLAN's record of the node, and the gate
// values each stage whose outs gate it posted at the fork they share, keyed
// by the instance's ordinals of the gate's own roots. Each instance carries
// on as its d the meta of its tasks, planned from the record and the
// node's own meta. PLAN's records reach it once its preflights are done,
// so no instance of it is gated before.
func (p *project) gate(w *wiring, i int, n cg.TopoNode, inst, meta string) {
	gated := fmt.Sprintf("GATE(%s, %s, %s)", inst, w.use(nodesAfter(p.preflightsOf(n))), meta)

	for _, gp := range n.Gates {
		w.linef("g%d = %s", i, gated)

		q := p.index[gp.Node]
		gated = fmt.Sprintf("GATED(g%d, %s, %s, %s, %s)", i, w.use(gatesOf(q)), ilist(positions(n.Roots, p.topo.Nodes[q].Roots)),
			ilist(positions(n.Roots, gp.Roots)), gstr(cg.GateToken(n.Fqid, gp, "")))
	}

	w.linef("b%d = ENABLED(%s)", i, gated)
}

// read is what READ takes for each enabled instance of node i to read of
// node r.Node: a channel keyed by the roots they share, those roots'
// positions in node i's key, and the outputs it reads of the channel's
// items. The channel is the upstream's outs, keyed by its whole key, when
// node i shares every root of it, or, when the upstream is forked on roots
// node i is not, a gather of the outputs it reads of every such instance,
// ordered by the upstream's key, not by completion, which waits for all of
// the upstream, as mrp waits for every fork of a node a node reads.
func (p *project) read(w *wiring, i int, n cg.TopoNode, r cg.Read) string {
	u := p.index[r.Node]

	var mine, theirs []int

	gather := false

	for x, root := range p.topo.Nodes[u].Roots {
		if at := slices.Index(n.Roots, root); at >= 0 {
			mine, theirs = append(mine, at), append(theirs, x)
		} else {
			gather = true
		}
	}

	fields := "null"
	if r.Fields != nil {
		fields = glist(r.Fields)
	}

	outs := w.use(outsOf(u))
	if gather {
		return fmt.Sprintf("GATHER(b%d.run, %s, %s, %s, %s), %s, null", i, outs, ilist(mine), ilist(theirs), fields, ilist(mine))
	}

	return fmt.Sprintf("%s, %s, %s", outs, ilist(mine), fields)
}

// splitTriad runs split node i's instances, in: its split, a main per
// chunk the split returned, and its join over every chunk's outs, j<i>,
// each task staging the instance's inputs.
func splitTriad(w *wiring, i int, proc, in string) {
	w.incs = append(w.incs, fmt.Sprintf("STAGE_SPLIT as %[1]s_SP ; STAGE_MAIN as %[1]s_MN ; STAGE_JOIN as %[1]s_JN", proc))
	w.linef("s%d = %s", i, in)
	w.linef("%s_SP(s%d)", proc, i)
	w.linef("%[1]s_MN(CHUNKS(%[1]s_SP.out.split, s%[2]d))", proc, i)
	w.linef("j%[2]d = JOIN(%[1]s_SP.out.split, %[1]s_MN.out.chunk, s%[2]d)", proc, i)
	w.linef("%[1]s_JN(j%[2]d)", proc, i)
}
