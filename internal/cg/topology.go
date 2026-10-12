package cg

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/martian-lang/martian/martian/syntax"
)

// errShapeUnsupported reports a call graph shape the emitted project does
// not wire.
var errShapeUnsupported = errors.New("unsupported call graph shape")

// Topology is what a run's tasks exchange on a call graph: per stage node,
// its fork roots, the stage outs it reads and the stages whose outs disable
// it; per fork root, what feeds its forks; and what the pipeline outs read.
// Nodes are in dependency order: each follows every node it reads, the
// producers of its fork roots and its gates.
type Topology struct {
	Nodes []TopoNode `json:"nodes"`
	Roots []TopoRoot `json:"roots"`
	Outs  []Read     `json:"outs"`
}

// TopoNode is one stage node. Gates are the stages whose outs one of its
// disable bindings reads; a binding that reads none is the invocation's.
// Off marks a node the invocation disables outright, which runs nothing at
// any fork and whose outs every consumer already reads as null. Zips are
// the outputs of other stages it splits over one of its roots whose forks
// they do not fix, each staged as the one partner element at its index.
type TopoNode struct {
	Fqid  string   `json:"fqid"`
	Stage string   `json:"stage"`
	Split bool     `json:"split"`
	Roots []string `json:"roots"`
	Reads []Read   `json:"reads"`
	Zips  []Zip    `json:"zips"`
	Gates []Gate   `json:"gates"`
	Off   bool     `json:"off"`
}

// Zip is a node's split over its root Root of node Node's output Output,
// which Node's post writes element by element (partner).
type Zip struct {
	Root   string `json:"root"`
	Node   string `json:"node"`
	Output string `json:"output"`
}

// Gate is a stage whose outs a disable binding of a node reads, and the
// node's roots, beyond the stage's own, the binding is forked on: a split
// of the stage's outs over a map call the stage feeds. The stage's post
// posts the binding's value at each of their forks (GateToken).
type Gate struct {
	Node  string   `json:"node"`
	Roots []string `json:"roots"`
}

// GateToken names, in a gating stage's posted gates, the instances of node
// whose binding of gate holds at the ordinals key of gate's roots.
func GateToken(node string, gate Gate, key string) string {
	return node + "@" + strings.Join(gate.Roots, ",") + "@" + key
}

// Read is the outs of one stage node a task stages: the named outputs, or
// all of them when Fields is nil (a whole-call reference).
type Read struct {
	Node   string   `json:"node"`
	Fields []string `json:"fields"`
}

// TopoRoot is one fork root: the outer roots its fork set varies with, the
// stage whose outs fix its forks, or "" when the invocation's entry values
// do, and whether its forks are a map's keys, not an array's positions.
type TopoRoot struct {
	Fqid     string   `json:"fqid"`
	Outer    []string `json:"outer"`
	Producer string   `json:"producer"`
	Keyed    bool     `json:"keyed"`
}

// Topology computes g's topology, failing on a shape whose data the
// stage tasks cannot exchange: a fork root whose forks several stages fix,
// a disable binding that reads several stages, or a stage forked on roots
// the gated node is not, or is forked on a root whose forks that stage
// cannot enumerate (gateRoots), or a fork root's outer root that a node
// forked on it is not forked on first.
func (g *Graph) Topology() (Topology, error) {
	t := Topology{}

	for _, fqid := range slices.Sorted(maps.Keys(g.roots)) {
		r, err := g.topoRoot(g.roots[fqid])
		if err != nil {
			return Topology{}, err
		}

		t.Roots = append(t.Roots, r)
	}

	byID := map[string]TopoNode{}

	for fqid, st := range g.stages {
		n, err := g.topoNode(st)
		if err != nil {
			return Topology{}, err
		}

		byID[fqid] = n
	}

	t.Nodes = dependencyOrder(byID, t.Roots)

	if ro := g.root.ResolvedOutputs(); ro != nil && ro.Exp != nil {
		t.Outs = reads(ro.Exp)
	}

	return t, nil
}

func (g *Graph) topoRoot(root *syntax.CallGraphStage) (TopoRoot, error) {
	if runtimeMultiSource(root) {
		return TopoRoot{}, fmt.Errorf("%w: the forks of %s are fixed by the outs of %s", errShapeUnsupported, root.Fqid, strings.Join(refIDs(root.Split().Value), " and "))
	}

	sp := root.Split()
	r := TopoRoot{Fqid: root.Fqid, Outer: fqids(g.outerRoots(root)), Keyed: sp != nil && sp.Source.CallMode() == syntax.ModeMapCall}
	r.Producer, _ = g.Producer(root)

	return r, nil
}

func (g *Graph) topoNode(st *syntax.CallGraphStage) (TopoNode, error) {
	s, ok := st.Callable().(*syntax.Stage)
	if !ok {
		return TopoNode{}, fmt.Errorf("%s: %w: not a stage", st.Fqid, errShapeUnsupported)
	}

	n := TopoNode{Fqid: st.Fqid, Stage: s.Id, Split: s.Split, Roots: fqids(st.ForkRoots())}

	for i, r := range st.ForkRoots() {
		for _, o := range g.outerRoots(r) {
			if !slices.Contains(n.Roots[:i], o.Fqid) {
				return TopoNode{}, fmt.Errorf("%w: %s is forked on %s before %s, whose forks vary with it", errShapeUnsupported, st.Fqid, r.Fqid, o.Fqid)
			}
		}
	}

	n.Reads, n.Zips = g.nodeReads(st)

	gates, err := g.gates(st)
	if err != nil {
		return TopoNode{}, err
	}

	n.Gates = gates
	n.Off = slices.ContainsFunc(st.Disabled(), func(d syntax.Exp) bool {
		b, ok := d.(*syntax.BoolExp)

		return ok && b.Value
	})

	return n, nil
}

// nodeReads are the stage outs st's bindings read, less the splits a
// staged element serves: of an output of the stage that feeds the split's
// root (the root's fork element), and, in an input binding, of another
// stage's output (its partner element, one of the zips). The gating stage
// posts a disable binding's value, so no partner element serves its split.
func (g *Graph) nodeReads(st *syntax.CallGraphStage) ([]Read, []Zip) {
	elementOnly := map[*syntax.CallStm]string{}

	for _, r := range st.ForkRoots() {
		if p, ok := g.Producer(r); ok {
			elementOnly[r.Call()] = p
		}
	}

	element := func(x *syntax.SplitExp) bool { return elementServed(x, elementOnly) }
	input := func(x *syntax.SplitExp) bool {
		_, partnered := g.partnerOf(st, x)

		return partnered || element(x)
	}

	fields := map[string]map[string]bool{}
	zips := map[Zip]bool{}

	for _, k := range slices.Sorted(maps.Keys(st.ResolvedInputs())) {
		e := st.ResolvedInputs()[k].Exp
		collectReads(e, input, fields)
		walkSplits(e, func(x *syntax.SplitExp) {
			if p, ok := g.partnerOf(st, x); ok {
				zips[Zip{Root: p.root.Fqid, Node: p.ref.Id, Output: p.ref.OutputId}] = true
			}
		})
	}

	for _, d := range st.Disabled() {
		collectReads(d, element, fields)
	}

	return readList(fields), slices.SortedFunc(maps.Keys(zips), compareZips)
}

func compareZips(a, b Zip) int {
	return strings.Compare(PartnerDir(a.Root, a.Node, a.Output), PartnerDir(b.Root, b.Node, b.Output))
}

// gates are the stages whose outs one of st's disable bindings reads, each
// with the roots the binding is forked on beyond the stage's.
func (g *Graph) gates(st *syntax.CallGraphStage) ([]Gate, error) {
	var out []Gate

	for _, d := range st.Disabled() {
		refs := refIDs(d)

		switch {
		case len(refs) == 0:
			continue
		case len(refs) > 1:
			return nil, fmt.Errorf("%w: a disable binding of %s reads %s", errShapeUnsupported, st.Fqid, strings.Join(refs, " and "))
		}

		up, err := g.Stage(refs[0])
		if err != nil {
			return nil, err
		}

		for _, r := range up.ForkRoots() {
			if !slices.ContainsFunc(st.ForkRoots(), func(x *syntax.CallGraphStage) bool { return x.Fqid == r.Fqid }) {
				return nil, fmt.Errorf("%w: %s is disabled by %s, which is forked on %s and it is not", errShapeUnsupported, st.Fqid, up.Fqid, r.Fqid)
			}
		}

		extra, err := g.gateRoots(st, d, up)
		if err != nil {
			return nil, err
		}

		gate := Gate{Node: up.Fqid, Roots: fqids(extra)}
		if !slices.ContainsFunc(out, gate.equal) {
			out = append(out, gate)
		}
	}

	slices.SortFunc(out, func(a, b Gate) int {
		return strings.Compare(a.Node+"\x00"+strings.Join(a.Roots, ","), b.Node+"\x00"+strings.Join(b.Roots, ","))
	})

	return out, nil
}

func (gate Gate) equal(o Gate) bool {
	return gate.Node == o.Node && slices.Equal(gate.Roots, o.Roots)
}

// gateRoots are the roots of st, in its order, that d, a disable binding
// of st reading up, is forked on and up is not. up's post enumerates their
// forks, so each must be fed by up or fixed by the entry, and vary with no
// root up's fork and the roots before it do not fix.
func (g *Graph) gateRoots(st *syntax.CallGraphStage, d syntax.Exp, up *syntax.CallGraphStage) ([]*syntax.CallGraphStage, error) {
	calls := map[*syntax.CallStm]bool{}
	splitCalls(d, calls)

	known := slices.Clone(up.ForkRoots())

	var out []*syntax.CallGraphStage

	for _, r := range st.ForkRoots() {
		if !calls[r.Call()] || slices.Contains(known, r) {
			continue
		}

		if p, ok := g.Producer(r); ok && p != up.Fqid {
			return nil, fmt.Errorf("%w: a disable binding of %s reads %s and is forked on %s, whose forks %s fixes", errShapeUnsupported, st.Fqid, up.Fqid, r.Fqid, p)
		}

		for _, o := range g.outerRoots(r) {
			if !slices.Contains(known, o) {
				return nil, fmt.Errorf("%w: a disable binding of %s is forked on %s, whose forks vary with %s, which %s is not forked on", errShapeUnsupported, st.Fqid, r.Fqid, o.Fqid, up.Fqid)
			}
		}

		known = append(known, r)
		out = append(out, r)
	}

	return out, nil
}

// reads are the stage outs exps read.
func reads(exps ...syntax.Exp) []Read {
	fields := map[string]map[string]bool{}

	for _, e := range exps {
		collectReads(e, nil, fields)
	}

	return readList(fields)
}

// readList lists the reads fields records, by node.
func readList(fields map[string]map[string]bool) []Read {
	out := make([]Read, 0, len(fields))

	for _, node := range slices.Sorted(maps.Keys(fields)) {
		r := Read{Node: node}
		if !fields[node][""] {
			r.Fields = slices.Sorted(maps.Keys(fields[node]))
		}

		out = append(out, r)
	}

	return out
}

// collectReads adds to fields the stage outs e reads, less the splits
// served reports, which a staged element serves.
func collectReads(e syntax.Exp, served func(*syntax.SplitExp) bool, fields map[string]map[string]bool) {
	switch x := e.(type) {
	case *syntax.SplitExp:
		if served == nil || !served(x) {
			collectReads(x.Value, served, fields)
		}
	case *syntax.RefExp:
		addRead(fields, x)
	case *syntax.ArrayExp:
		for _, v := range x.Value {
			collectReads(v, served, fields)
		}
	case *syntax.MapExp:
		for _, v := range x.Value {
			collectReads(v, served, fields)
		}
	case *syntax.MergeExp:
		collectReads(x.Value, served, fields)

		if x.ForkNode != nil {
			addRead(fields, x.ForkNode)
		}

		// A merge over a root whose forks a stage fixes takes them from the
		// root's source, as mrp does: the instances it gathers are no
		// count of them, as a disabled one posts none.
		if sp := x.Call.Split(); sp != nil && !sp.Source.KnownLength() {
			collectReads(sp.Value, nil, fields)
		}
	case *syntax.DisabledExp:
		collectReads(x.Disabled, served, fields)
		collectReads(x.Value, served, fields)
	}
}

// elementServed reports a split of an output of the stage that feeds its
// root, whose element the root's fork element carries (zipSources).
func elementServed(x *syntax.SplitExp, elementOnly map[*syntax.CallStm]string) bool {
	ref, ok := x.Value.(*syntax.RefExp)

	return ok && elementOnly[x.Call] != "" && ref.Id == elementOnly[x.Call]
}

// addRead records the top-level output a reference reads, "" for the whole
// call.
func addRead(fields map[string]map[string]bool, ref *syntax.RefExp) {
	if ref.Kind == syntax.KindSelf {
		return
	}

	if fields[ref.Id] == nil {
		fields[ref.Id] = map[string]bool{}
	}

	field, _, _ := strings.Cut(ref.OutputId, ".")
	fields[ref.Id][field] = true
}

// errUncovered reports a run's call graph that holds what the witness's
// lacks, which the transpiled project has no task or channel for.
var errUncovered = errors.New("the invocation's call graph is not within the transpiled one")

// Covers checks that every node, fork root and read of r is one of t's: the
// stage nodes of r with the same stage and fork roots, reading no stage
// outs and gated by no stage t's does not, and every fork root fed by the
// same producer, varying with the same outer roots. An entry-fed root may
// vary with fewer: nested entry collections all of one length are a source
// of known length, which varies with none (syntax InnerMapSource), and the
// plan repeats its forks under every outer fork (entryForks). Of a node r
// disables outright only its presence counts, and so do the roots only such
// nodes are forked on: it runs at no fork.
func (t Topology) Covers(r Topology) error {
	errs, live := t.coverNodes(r.Nodes)

	for _, rr := range r.Roots {
		if live[rr.Fqid] && !slices.ContainsFunc(t.Roots, func(x TopoRoot) bool {
			return x.Fqid == rr.Fqid && x.Producer == rr.Producer &&
				(slices.Equal(x.Outer, rr.Outer) || (rr.Producer == "" && subset(x.Outer, rr.Outer)))
		}) {
			errs = append(errs, fmt.Errorf("%w: fork root %s fed by %q varying with %v", errUncovered, rr.Fqid, rr.Producer, rr.Outer))
		}
	}

	if !readsCovered(t.Outs, r.Outs) {
		errs = append(errs, fmt.Errorf("%w: pipeline outs reading %v, beyond %v", errUncovered, r.Outs, t.Outs))
	}

	return errors.Join(errs...)
}

// coverNodes checks the nodes of r as Covers does, and returns the roots a
// node r does not disable outright is forked on.
func (t Topology) coverNodes(rs []TopoNode) ([]error, map[string]bool) {
	nodes := map[string]TopoNode{}
	for _, n := range t.Nodes {
		nodes[n.Fqid] = n
	}

	var errs []error

	live := map[string]bool{}

	for _, rn := range rs {
		n, ok := nodes[rn.Fqid]

		switch {
		case !ok:
			errs = append(errs, fmt.Errorf("%w: stage node %s", errUncovered, rn.Fqid))
		case rn.Off:
		case n.Stage != rn.Stage || n.Split != rn.Split || !slices.Equal(n.Roots, rn.Roots):
			errs = append(errs, fmt.Errorf("%w: %s is stage %s forked on %v, not %s forked on %v", errUncovered, rn.Fqid, rn.Stage, rn.Roots, n.Stage, n.Roots))
		case !readsCovered(n.Reads, rn.Reads) || !gatesCovered(n.Gates, rn.Gates):
			errs = append(errs, fmt.Errorf("%w: %s reads %v and is gated by %v, beyond %v and %v", errUncovered, rn.Fqid, rn.Reads, rn.Gates, n.Reads, n.Gates))
		case !subset(n.Zips, rn.Zips):
			errs = append(errs, fmt.Errorf("%w: %s zips %v, beyond %v", errUncovered, rn.Fqid, rn.Zips, n.Zips))
		}

		for _, root := range rn.Roots {
			live[root] = live[root] || !rn.Off
		}
	}

	return errs, live
}

func readsCovered(have, want []Read) bool {
	for _, w := range want {
		i := slices.IndexFunc(have, func(h Read) bool { return h.Node == w.Node })
		if i < 0 || (have[i].Fields != nil && (w.Fields == nil || !subset(have[i].Fields, w.Fields))) {
			return false
		}
	}

	return true
}

func gatesCovered(have, want []Gate) bool {
	for _, w := range want {
		if !slices.ContainsFunc(have, w.equal) {
			return false
		}
	}

	return true
}

func subset[T comparable](have, want []T) bool {
	for _, w := range want {
		if !slices.Contains(have, w) {
			return false
		}
	}

	return true
}

// dependencyOrder lists the nodes so each follows what it reads, the
// producers of its fork roots, its gates and the stages it zips, visiting
// in fqid order.
func dependencyOrder(nodes map[string]TopoNode, roots []TopoRoot) []TopoNode {
	producer := map[string]string{}
	for _, r := range roots {
		producer[r.Fqid] = r.Producer
	}

	done := map[string]bool{}
	out := make([]TopoNode, 0, len(nodes))

	var visit func(string)

	visit = func(id string) {
		n, ok := nodes[id]
		if !ok || done[id] {
			return
		}

		done[id] = true

		for _, r := range n.Reads {
			visit(r.Node)
		}

		for _, r := range n.Roots {
			visit(producer[r])
		}

		for _, gate := range n.Gates {
			visit(gate.Node)
		}

		for _, z := range n.Zips {
			visit(z.Node)
		}

		out = append(out, n)
	}

	for _, id := range slices.Sorted(maps.Keys(nodes)) {
		visit(id)
	}

	return out
}

func fqids(roots syntax.ForkRootList) []string {
	out := make([]string, len(roots))
	for i, r := range roots {
		out[i] = r.Fqid
	}

	return out
}
