package cg

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/martian-lang/martian/martian/core"
	"github.com/martian-lang/martian/martian/syntax"
)

// Plan is what a run's start fixes once its invocation is known: the call
// every task compiles over the entry's declarations (Load), the entry file
// values tasks stage (EntryLeaves), per stage node of the witness, the
// node's spec digest, the instances the invocation itself disables and the
// entry files each instance reads, and the forks of every fork root the
// entry values feed. Outs is the pipeline, by its fqid, and the entry
// files its outs read, at the one instance, keyed "".
type Plan struct {
	Call   string
	Leaves []EntryLeaf
	Nodes  []PlanNode
	Outs   PlanNode
	Roots  map[string][]PlanFork
}

// PlanNode is one witness node at run start. Digest identifies the node's
// resolved spec in the invocation and what its post writes for its readers,
// so a task's cache key changes only with its own node and those. Off
// disables every instance; OffAt the instances a static per-fork binding
// disables, by their ordinals of the witness node's roots at the positions
// OffRoots, those the binding varies with. Collapse are the positions of
// the witness node's roots the invocation's node is not forked on: its job
// is the same at every fork of them, which runs at their ordinal 0 only and
// serves the rest. Files are the entry files its instances read. Stage is
// the stage it runs, which the pipeline outs' node has none of.
type PlanNode struct {
	Fqid     string
	Digest   string
	Off      bool
	OffAt    []string
	OffRoots []int
	Collapse []int
	Files    []PlanFiles
	Stage    *syntax.Stage
}

// PlanFork is one fork of an entry-fed root: its fork key, the ordinals of
// the witness's outer roots then its own, its fork record as a value
// (EncodeElement), and its index's IndexToken.
type PlanFork struct {
	Key    []int
	Record string
	Token  string
}

// MakePlan compiles inv over the source as mrp compiles an invocation, and
// checks that its call graph is within the witness's, which the project was
// transpiled from. scratch receives the entry's fork elements. No instance
// stages an entry file value in reads in place; it keeps its index among
// the Leaves, which a task recomputes from the call.
func MakePlan(src Source, inv *core.InvocationData, mroPaths []string, scratch string, in ReadInPlace) (Plan, error) {
	w, wt, err := src.Witness(mroPaths)
	if err != nil {
		return Plan{}, err
	}

	call, err := src.RebuildCall(inv, mroPaths)
	if err != nil {
		return Plan{}, err
	}

	g, err := Compile([]byte(src.with(call)), src.Path(), mroPaths)
	if err != nil {
		return Plan{}, err
	}

	rt, err := g.Topology()
	if err != nil {
		return Plan{}, err
	}

	if err := wt.Covers(rt); err != nil {
		return Plan{}, err
	}

	p := Plan{Call: call}

	if p.Leaves, err = g.EntryLeaves(); err != nil {
		return Plan{}, err
	}

	roots, planned, err := g.entryForks(wt, scratch)
	if err != nil {
		return Plan{}, err
	}

	p.Roots = roots

	leaves := make(map[string]int, len(p.Leaves))

	for i := range p.Leaves {
		p.Leaves[i].InPlace = in.Holds(p.Leaves[i].Value)
		if !p.Leaves[i].InPlace {
			leaves[p.Leaves[i].Value] = i
		}
	}

	for _, n := range wt.Nodes {
		pn, err := g.planNode(n, wt, planned, leaves)
		if err != nil {
			return Plan{}, err
		}

		if pn.Stage, err = w.decl(n.Fqid); err != nil {
			return Plan{}, err
		}

		p.Nodes = append(p.Nodes, pn)
	}

	if p.Outs, err = g.planOuts(leaves); err != nil {
		return Plan{}, err
	}

	return p, nil
}

// planOuts is the pipeline and the entry files its outs read.
func (g *Graph) planOuts(leaves map[string]int) (PlanNode, error) {
	pn := PlanNode{Fqid: g.Fqid()}

	outs, err := g.outsEntryFiles(leaves)
	if err != nil {
		return PlanNode{}, err
	}

	if len(outs) > 0 {
		pn.Files = []PlanFiles{{Leaves: outs}}
	}

	return pn, nil
}

// EncodeElement is el as a fork record value, DecodeElement's text.
func EncodeElement(el Element) (string, error) {
	raw, err := json.Marshal(el.encode())
	if err != nil {
		return "", fmt.Errorf("encode fork record: %w", err)
	}

	return base64.StdEncoding.EncodeToString(raw), nil
}

// entryForks are the forks of the witness's entry-fed roots, and of its
// stage-fed roots the invocation's entry fixes (TopoRoot.Entry), keyed by
// the witness's outer roots: a root the invocation gives a fork set of
// known length repeats it under every fork of the outer roots the
// witness's varies with.
func (g *Graph) entryForks(wt Topology, scratch string) (map[string][]PlanFork, *plannedForks, error) {
	dir := filepath.Join(scratch, "entry")
	if err := g.PostEntry(dir); err != nil {
		return nil, nil, err
	}

	own := map[string][]Element{}
	roots := slices.Clone(wt.Roots)

	for x, r := range roots {
		if r.Producer != "" {
			if root, ok := g.roots[r.Fqid]; !r.Entry || !ok || len(g.rootCountRefs(root)) > 0 {
				continue
			}

			roots[x].Producer = ""
		}

		els, err := g.readElements(filepath.Join(dir, r.Fqid))
		if err != nil {
			return nil, nil, err
		}

		own[r.Fqid] = els
	}

	return planEntry(roots, own)
}

// planEntry plans the forks own holds of each entry-fed root, outer roots
// first. A root's forks under each fork of its outer roots are looked up,
// not scanned for, so planning is linear in the forks however they nest.
func planEntry(wtRoots []TopoRoot, own map[string][]Element) (map[string][]PlanFork, *plannedForks, error) {
	roots := slices.Clone(wtRoots)
	sort.SliceStable(roots, func(i, j int) bool { return len(roots[i].Outer) < len(roots[j].Outer) })

	planned := &plannedForks{by: map[string][]entryFork{}, idx: map[string]*agreeIndex{}}
	out := map[string][]PlanFork{}

	for _, r := range roots {
		if r.Producer != "" {
			continue
		}

		pr, err := planned.plan(r, own[r.Fqid])
		if err != nil {
			return nil, nil, err
		}

		out[r.Fqid] = pr
	}

	return out, planned, nil
}

// plannedForks are the planned forks of each entry-fed root, an entry each
// however few, and an index of each root's, built when a root its outer
// roots include is planned, once its own forks are all planned.
type plannedForks struct {
	by  map[string][]entryFork
	idx map[string]*agreeIndex
}

// has reports whether root is entry-fed, so planned.
func (p *plannedForks) has(root string) bool {
	_, ok := p.by[root]

	return ok
}

// plan plans root r's forks own, under each fork of its outer roots.
func (p *plannedForks) plan(r TopoRoot, own []Element) ([]PlanFork, error) {
	p.by[r.Fqid] = []entryFork{}

	items := make([]agreeItem, len(own))
	for i, el := range own {
		items[i] = agreeItem{fork: el.Fork}
	}

	byFork := newAgreeIndex(items)
	forks := []PlanFork{}

	for _, c := range assignments(r.Outer, p) {
		for n, i := range byFork.agreeing(nil, c.fork) {
			el := own[i]

			rec, err := EncodeElement(el)
			if err != nil {
				return nil, err
			}

			ef := entryFork{el: el, ords: c.with(r.Fqid, n)}
			p.by[r.Fqid] = append(p.by[r.Fqid], ef)
			forks = append(forks, PlanFork{
				Key:    ef.key(append(slices.Clone(r.Outer), r.Fqid)),
				Record: rec,
				Token:  IndexToken(el.Fork[len(el.Fork)-1].Index),
			})
		}
	}

	return forks, nil
}

// index is the index of root's planned forks.
func (p *plannedForks) index(root string) *agreeIndex {
	if x, ok := p.idx[root]; ok {
		return x
	}

	items := make([]agreeItem, len(p.by[root]))
	for i, f := range p.by[root] {
		items[i] = agreeItem{ords: f.ords, fork: f.el.Fork}
	}

	x := newAgreeIndex(items)
	p.idx[root] = x

	return x
}

// entryFork is one planned fork of an entry-fed root: its element, and the
// ordinal of each root its key names, by root.
type entryFork struct {
	el   Element
	ords map[string]int
}

func (f entryFork) key(roots []string) []int {
	out := make([]int, len(roots))
	for i, r := range roots {
		out[i] = f.ords[r]
	}

	return out
}

// assignment is one fork of a list of roots: each root's ordinal and the
// fork's parts.
type assignment struct {
	ords map[string]int
	fork Fork
}

func (a assignment) with(root string, ord int) map[string]int {
	out := make(map[string]int, len(a.ords)+1)
	maps.Copy(out, a.ords)

	out[root] = ord

	return out
}

// assignments enumerates the forks of roots, in order, from the forks
// planned for each: a fork of a root joins those of the roots before it
// whose ordinals and parts it agrees with.
func assignments(roots []string, planned *plannedForks) []assignment {
	out := []assignment{{ords: map[string]int{}}}

	for _, root := range roots {
		x := planned.index(root)

		var next []assignment

		for _, a := range out {
			for _, i := range x.agreeing(a.ords, a.fork) {
				f := planned.by[root][i]
				next = append(next, assignment{ords: mergeOrds(a.ords, f.ords), fork: mergeForks(a.fork, f.el.Fork)})
			}
		}

		out = next
	}

	return out
}

func mergeOrds(a, b map[string]int) map[string]int {
	out := make(map[string]int, len(a)+len(b))
	maps.Copy(out, a)

	maps.Copy(out, b)

	return out
}

// readElements reads a root's entry fork elements in fork order; a root
// the invocation prunes has none.
func (g *Graph) readElements(dir string) ([]Element, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("read entry forks %s: %w", dir, err)
	}

	out := make([]Element, 0, len(entries))

	for _, e := range entries {
		el, err := g.ReadElement(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}

		out = append(out, el)
	}

	return out, nil
}

func mergeForks(a, b Fork) Fork {
	out := slices.Clone(a)

	for _, p := range b {
		if _, ok := out.rootPart(p.Root); !ok {
			out = append(out, p)
		}
	}

	return out
}

// planNode is the plan of the witness node n in the invocation's graph,
// whose entry file values leaves indexes.
func (g *Graph) planNode(n TopoNode, wt Topology, planned *plannedForks, leaves map[string]int) (PlanNode, error) {
	st, ok := g.stages[n.Fqid]
	if !ok {
		return PlanNode{Fqid: n.Fqid, Off: true}, nil
	}

	pn := PlanNode{Fqid: n.Fqid}

	var err error

	if pn.Digest, err = g.specDigest(st); err != nil {
		return PlanNode{}, err
	}

	pn.Off = slices.ContainsFunc(st.Disabled(), func(d syntax.Exp) bool {
		b, ok := d.(*syntax.BoolExp)

		return ok && b.Value
	})

	if pn.Off {
		return pn, nil
	}

	for x, root := range n.Roots {
		if !slices.ContainsFunc(st.ForkRoots(), func(r *syntax.CallGraphStage) bool { return r.Fqid == root }) {
			pn.Collapse = append(pn.Collapse, x)
		}
	}

	if pn.OffAt, pn.OffRoots, err = g.staticOff(st, n, planned); err != nil {
		return PlanNode{}, err
	}

	if pn.Files, err = g.entryFiles(st, n, wt, planned, leaves); err != nil {
		return PlanNode{}, err
	}

	return pn, nil
}

// staticOff are the instances of st a disable binding that reads no stage
// disables, keyed by their ordinals of the roots at the positions in n's
// roots it returns, those the bindings vary with (offRoots): a binding's
// value at a fork is the same whatever its fork of any other root, such as
// one whose forks a stage fixes (core/stage.go:883 Fork.disabled resolves
// it at the fork). A binding that does not resolve to a bool is left to
// the instance's own task, which fails on it as mrp fails the fork.
func (g *Graph) staticOff(st *syntax.CallGraphStage, n TopoNode, planned *plannedForks) ([]string, []int, error) {
	var static []syntax.Exp

	calls := map[*syntax.CallStm]bool{}

	for _, d := range st.Disabled() {
		if _, isBool := d.(*syntax.BoolExp); !isBool && len(refIDs(d)) == 0 {
			static = append(static, d)
			splitCalls(d, calls)
		}
	}

	at, err := g.offRoots(st, n, planned, calls)
	if err != nil || len(at) == 0 {
		return nil, nil, err
	}

	roots := make([]string, len(at))
	for i, x := range at {
		roots[i] = n.Roots[x]
	}

	r := &resolver{g: g, st: emptyStore{}}

	var off []string

	for _, in := range assignments(roots, planned) {
		if slices.ContainsFunc(static, func(d syntax.Exp) bool {
			on, err := r.disables(d, in.fork)

			return err == nil && on
		}) {
			off = append(off, DottedKey(entryFork{ords: in.ords}.key(roots)))
		}
	}

	return off, at, nil
}

// offRoots are the positions in n's roots of the roots st is forked on
// that a split of calls is on, and of the roots their forks vary with,
// each of which the entry must fix for PLAN to enumerate its forks.
func (g *Graph) offRoots(st *syntax.CallGraphStage, n TopoNode, planned *plannedForks, calls map[*syntax.CallStm]bool) ([]int, error) {
	bound := map[string]bool{}

	var add func(r *syntax.CallGraphStage)

	add = func(r *syntax.CallGraphStage) {
		if !bound[r.Fqid] {
			bound[r.Fqid] = true
			for _, o := range g.outerRoots(r) {
				add(o)
			}
		}
	}

	for _, r := range st.ForkRoots() {
		if calls[r.Call()] {
			add(r)
		}
	}

	var at []int

	for x, root := range n.Roots {
		if !bound[root] {
			continue
		}

		if !planned.has(root) {
			return nil, fmt.Errorf("%w: a disable binding of %s is forked on %s, whose forks a stage fixes", ErrShapeUnsupported, st.Fqid, root)
		}

		at = append(at, x)
	}

	return at, nil
}

// DottedKey is a fork key as the plan's records write it and the driver
// matches it, `k.join('.')`: its ordinals joined by dots.
func DottedKey(key []int) string {
	parts := make([]string, len(key))
	for i, k := range key {
		parts[i] = strconv.Itoa(k)
	}

	return strings.Join(parts, ".")
}

// decl is the stage node fqid runs.
func (g *Graph) decl(fqid string) (*syntax.Stage, error) {
	st, err := g.Stage(fqid)
	if err != nil {
		return nil, err
	}

	s, ok := st.Callable().(*syntax.Stage)
	if !ok {
		return nil, fmt.Errorf("%s: %w: not a stage", fqid, ErrShapeUnsupported)
	}

	return s, nil
}

// specDigest identifies a stage node's resolved spec: its stage's code,
// resources and parameters, its bindings, disable bindings and fork roots
// in Martian's own JSON form of a call graph, and what its post writes for
// its readers.
func (g *Graph) specDigest(st *syntax.CallGraphStage) (string, error) {
	s, ok := st.Callable().(*syntax.Stage)
	if !ok {
		return "", fmt.Errorf("%s: %w: not a stage", st.Fqid, ErrShapeUnsupported)
	}

	readers, err := g.postReaders(st)
	if err != nil {
		return "", err
	}

	spec := map[string]any{
		"readers":   readers,
		"stage":     s.Id,
		"split":     s.Split,
		"src":       []any{s.Src.Type, s.Src.Path, s.Src.Args},
		"in":        inTypes(s.InParams),
		"out":       outTypes(s.OutParams),
		"chunk_in":  inTypes(s.ChunkIns),
		"chunk_out": outTypes(s.ChunkOuts),
		"bindings":  st.ResolvedInputs(),
		"disabled":  st.Disabled(),
		"roots":     st.ForkRoots(),
	}

	if r := s.Resources; r != nil {
		spec["resources"] = []any{r.Threads, r.MemGB, r.VMemGB, r.Special, r.StrictVolatile}
	}

	raw, err := json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("spec of %s: %w", st.Fqid, err)
	}

	sum := sha256.Sum256(raw)

	return hex.EncodeToString(sum[:]), nil
}

func inTypes(ps *syntax.InParams) []string {
	if ps == nil {
		return nil
	}

	return paramTypes(ps.List)
}

func outTypes(ps *syntax.OutParams) []string {
	if ps == nil {
		return nil
	}

	return paramTypes(ps.List)
}

// paramTypes are a parameter list's names and types, without the source
// locations its declaration carries.
func paramTypes[P syntax.StructMemberLike](list []P) []string {
	out := make([]string, len(list))
	for i, p := range list {
		t := p.GetTname()
		out[i] = p.GetId() + " " + t.String()
	}

	return out
}
