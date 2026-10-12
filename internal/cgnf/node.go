package cgnf

import (
	"fmt"
	"slices"
	"strconv"

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

// The channels of PLAN's records for an entry-fed root.
func entryForksOf(j int) string  { return "f" + strconv.Itoa(j) }
func entryCountsOf(j int) string { return "n" + strconv.Itoa(j) }

// node wires node i: its instances, enumerated root by root from the fork
// elements and records its roots' feeders write, each held until its
// preflights are done; its gates, which the driver evaluates so a disabled
// instance runs no task; the outs its enabled instances stage, field by
// field; its process call; and its outputs, a disabled instance standing in
// with none.
func (p *project) node(i int, n cg.TopoNode) (*wiring, error) {
	w := &wiring{}
	w.linef("// %s", n.Fqid)
	p.gate(w, i, n, p.instances(w, i, n))

	run := fmt.Sprintf("b%d.run", i)
	for _, r := range n.Reads {
		w.linef("u%d = READ(%s, %s)", i, run, p.read(w, i, n, r))
		run = "u" + strconv.Itoa(i)
	}

	meta, err := p.nodeMeta(n)
	if err != nil {
		return nil, err
	}

	proc := alias(i, n.Fqid)
	in := p.input(w, n, run, meta)

	if n.Split {
		splitTriad(w, proc, in)
		proc += "_JN"
	} else {
		w.incs = append(w.incs, "STAGE as "+proc)
		w.linef("%s(%s)", proc, in)
	}

	if p.awaited(n.Fqid) {
		w.linef("%s = DONE(%s.out.outs)", w.def(doneOf(i)), proc)
	}

	outs, forks, gates := p.consumed(n.Fqid)
	for _, o := range []struct {
		used             bool
		name, out, empty string
	}{{outs, outsOf(i), "outs", "[]"}, {forks, forksOf(i), "forks", "[]"}, {gates, gatesOf(i), "gates", "''"}} {
		if o.used {
			w.linef("%s = OR_SKIPPED(%s.out.%s, b%d.skip, %s)", w.def(o.name), proc, o.out, i, o.empty)
		}
	}

	return w, nil
}

// input is the channel of node i's enabled instances, run, as its process
// takes them, each with the entry files it reads: those PLAN lists for its
// fork of the node's entry-fed roots.
func (p *project) input(w *wiring, n cg.TopoNode, run, meta string) string {
	if !p.readers[n.Fqid] {
		return fmt.Sprintf("INPUT(%s, %s, %s)", run, w.use("shared"), meta)
	}

	var at []int

	for x, r := range n.Roots {
		if p.roots[r].Producer == "" {
			at = append(at, x)
		}
	}

	return fmt.Sprintf("INPUT_ENTRY(%s, %s, %s, %s, %s)", run, w.use("shared"), w.use("entry"), meta, ilist(at))
}

// instances is the expression of node i's instances: root by root, each
// with the partner elements it zips, held until its preflights are done.
// Each step but the last is assigned to i<i>.
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

	for _, pf := range n.Preflights {
		step(fmt.Sprintf("AFTER(%s, %s)", inst, w.use(doneOf(p.index[pf]))))
	}

	return inst
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
// root's are the element dirs its producer posted. A root in zr takes its
// index token.
func (p *project) forkOn(w *wiring, inst string, n cg.TopoNode, root string, zr []string) string {
	r := p.roots[root]

	entryFed := r.Producer == ""

	var src string
	if entryFed {
		src = w.use(entryForksOf(slices.Index(p.entry, root)))
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

// elements is the channel of root's fork element dirs its producer posted:
// an outer root the producer is forked on takes the producer's ordinal for
// it, any other the ordinal the dir's name carries.
func (p *project) elements(w *wiring, r cg.TopoRoot) string {
	prod := p.index[r.Producer]

	return fmt.Sprintf("ELEMENTS(%s, %s, %s, %t)", w.use(forksOf(prod)), gstr(r.Fqid), ilist(positions(p.topo.Nodes[prod].Roots, r.Outer)), r.Keyed)
}

// gate decides each of node i's instances, inst, enabled or disabled,
// b<i>.run or b<i>.skip, from PLAN's record of the node, which each
// instance carries on as its d, and the gate values each stage whose outs
// gate it posted at the fork they share, keyed by the instance's ordinals
// of the gate's own roots.
func (p *project) gate(w *wiring, i int, n cg.TopoNode, inst string) {
	gated := fmt.Sprintf("GATE(%s, %s, %s)", inst, w.use("nodes"), gstr(n.Fqid))

	for _, gp := range n.Gates {
		w.linef("g%d = %s", i, gated)

		q := p.index[gp.Node]
		gated = fmt.Sprintf("GATED(g%d, %s, %s, %s, %s)", i, w.use(gatesOf(q)), ilist(positions(n.Roots, p.topo.Nodes[q].Roots)),
			ilist(positions(n.Roots, gp.Roots)), gstr(cg.GateToken(n.Fqid, gp, "")))
	}

	w.linef("b%d = ENABLED(%s)", i, gated)
}

// read is the channel of the outs bundles each enabled instance of node i
// reads of node r.Node, keyed by the roots they share, with those roots'
// positions in node i's key: from the instance agreeing on every root they
// share, or, when the upstream is forked on roots node i is not, every
// such instance, a gather ordered by the upstream's key, not by completion.
func (p *project) read(w *wiring, i int, n cg.TopoNode, r cg.Read) string {
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

	fields := "null"
	if r.Fields != nil {
		fields = glist(r.Fields)
	}

	outs, run := w.use(outsOf(u)), fmt.Sprintf("b%d.run", i)

	var src string

	switch counts, sized := p.forkCounts(w, n, up, extra); {
	case sized:
		src = p.sizedGather(n, up, run, outs, counts, fields)
	case len(extra) > 0:
		src = fmt.Sprintf("GATHER(%s, %s, %s, %s, %s)", run, outs, ilist(mine), ilist(theirs), fields)
	default:
		src = fmt.Sprintf("SELECT(%s, %s, %s)", outs, ilist(theirs), fields)
	}

	return src + ", " + ilist(mine)
}

// forkCounts is the channel of the fork counts of root extra[0] under each
// fork of its outer roots, [outer key, count], none left out, for a gather
// over it by a node n that is forked, each of whose instances then starts
// once its own share of the gather is in, not the whole of it. It is false
// for another gather: over several roots, or over one whose outer roots n
// does not share or whose producer's post enumerates them itself, whose
// counts no one item carries; such a gather waits for all of its upstream,
// as one consumed by an unforked node does by nature.
func (p *project) forkCounts(w *wiring, n, up cg.TopoNode, extra []string) (string, bool) {
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
		return w.use(entryCountsOf(slices.Index(p.entry, r.Fqid))), true
	}

	prod := p.index[r.Producer]

	at := positions(p.topo.Nodes[prod].Roots, r.Outer)
	if slices.Contains(at, -1) {
		return "", false
	}

	return fmt.Sprintf("COUNTS(%s, %s, %s)", w.use(forksOf(prod)), ilist(at), gstr(r.Fqid)), true
}

// sizedGather gathers, for each enabled instance of node n, run, the outs
// of every instance of up under it, by groupKey with their count, which
// counts gives for the outer fork of the gathered root the instance's key
// fixes.
func (p *project) sizedGather(n, up cg.TopoNode, run, outs, counts, fields string) string {
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

	return fmt.Sprintf("GATHER_SIZED(%s, %s, %s, %s, %s, %s, %s, %s)", run, outs, counts, ilist(mine), ilist(theirs), ilist(outerMine), ilist(outerTheirs), fields)
}

// splitTriad runs a split node: its split, a main per chunk the split
// returned, and its join over every chunk's outs.
func splitTriad(w *wiring, proc, in string) {
	w.incs = append(w.incs, fmt.Sprintf("STAGE_SPLIT as %[1]s_SP ; STAGE_MAIN as %[1]s_MN ; STAGE_JOIN as %[1]s_JN", proc))
	w.linef("%s_SP(%s)", proc, in)
	w.linef("%[1]s_MN(CHUNKS(%[1]s_SP.out.split))", proc)
	w.linef("%[1]s_JN(JOIN(%[1]s_SP.out.split, %[1]s_MN.out.chunk))", proc)
}
