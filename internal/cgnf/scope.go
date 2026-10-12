package cgnf

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// The JVM compiles each workflow's statements into one method, of at most
// 64 KiB of bytecode, and a call into one of at most 255 arguments.
// maxWeight bounds a workflow's weight: its statements by lineWeight, and
// portCost per take and emit; one weight unit compiles to at most about 10
// bytes, so a workflow of maxWeight is under half the limit. maxTakes
// bounds a workflow's takes, the arguments of its call.
const (
	maxWeight = 3000
	maxTakes  = 200
	portCost  = 3
)

var errCallCycle = errors.New("calls depend on each other")

// lineWeight is a statement's weight: a call, a list, a map entry and an
// argument each compile to a few instructions; a comment to none.
func lineWeight(line string) int {
	if strings.HasPrefix(line, "//") {
		return 0
	}

	w := 1
	for _, c := range ",([:" {
		w += strings.Count(line, string(c))
	}

	return w
}

func linesWeight(lines []string) int {
	w := 0
	for _, l := range lines {
		w += lineWeight(l)
	}

	return w
}

// scope is one workflow: a Martian pipeline's calls, or a run of them. It
// takes the channels its nodes read and do not define, and emits those it
// defines that a node outside it reads. Its published statement mixes the
// outs bundles its units publish for OUTS into one channel, which it emits
// too, so no workflow mixes more sources than it has units.
type scope struct {
	name, fqid   string
	part         bool
	units        []*unit
	nodes        []int
	takes, emits []string
	published    []string
}

// publishedChannel is the channel of a workflow's published statement. It
// is not "published": Nextflow 26.10 binds that name in every workflow as a
// Map, and assigning a channel to it fails the run.
const publishedChannel = "outs_pub"

// unit is the call of sub-workflow sub or, when sub is nil, node's wiring.
type unit struct {
	node int
	sub  *scope
}

// label is the pipeline s wires, or wires part of.
func (s *scope) label() string {
	if s.part {
		return "part of " + s.fqid
	}

	return s.fqid
}

// call is the statements that call s and name what it emits.
func (s *scope) call() []string {
	lines := make([]string, 0, 1+len(s.emits))
	lines = append(lines, s.name+"("+strings.Join(s.takes, ", ")+")")

	for _, e := range s.emits {
		lines = append(lines, e+" = "+s.name+".out."+e)
	}

	return lines
}

// layout places the nodes' wirings in workflows: the channel each defines,
// by its defining node, the nodes reading each, the order each is defined
// in, and the source of the outs bundles each node the pipeline outs read
// publishes.
type layout struct {
	nodes []*wiring
	owner map[string]int
	users map[string][]int
	order map[string]int
	pubs  map[int]string
	names int
}

// scopes lays the nodes' wirings out by the pipelines they are called
// from: the root pipeline's in the entry workflow, beside its plan and
// outs, and each sub-pipeline's in a workflow of its own, which calls its
// units in dependency order, as the calls of a Martian pipeline depend on
// each other acyclically.
func (p *project) scopes(nodes []*wiring, plan, outs *wiring) (*scope, error) {
	l := &layout{nodes: nodes, owner: map[string]int{}, users: map[string][]int{}, order: map[string]int{}, pubs: p.publications()}

	for _, d := range plan.defs {
		l.order[d] = len(l.order)
	}

	for i, w := range nodes {
		for _, d := range w.defs {
			l.owner[d], l.order[d] = i, len(l.order)
		}

		for _, u := range w.uses {
			l.users[u] = append(l.users[u], i)
		}
	}

	root := p.pipelineTree()
	if err := l.place(root, maxWeight-linesWeight(plan.lines)-linesWeight(outs.lines)); err != nil {
		return nil, err
	}

	l.name(root)
	l.publish(root)

	return root, nil
}

// pipelineTree is the scope of the root pipeline: its stage calls and a
// scope per sub-pipeline it calls, each the same, in node order.
func (p *project) pipelineTree() *scope {
	root := &scope{fqid: p.g.Fqid()}
	byFqid := map[string]*scope{}

	for i, n := range p.topo.Nodes {
		s := root
		s.nodes = append(s.nodes, i)
		parts := strings.Split(n.Fqid, ".")

		for d := 2; d < len(parts); d++ {
			fqid := strings.Join(parts[:d], ".")

			sub, ok := byFqid[fqid]
			if !ok {
				sub = &scope{fqid: fqid}
				byFqid[fqid] = sub
				s.units = append(s.units, &unit{sub: sub})
			}

			s = sub
			s.nodes = append(s.nodes, i)
		}

		s.units = append(s.units, &unit{node: i})
	}

	return root
}

// place lays s out, every scope below it first: a sub-pipeline whose call
// would take more than maxTakes channels is wired in s, unit by unit;
// then s's units are put in dependency order and, while they weigh more
// than budget, grouped into runs.
func (l *layout) place(s *scope, budget int) error {
	var units []*unit

	for _, u := range s.units {
		if u.sub == nil {
			units = append(units, u)

			continue
		}

		if err := l.place(u.sub, maxWeight); err != nil {
			return err
		}

		if len(u.sub.takes) > maxTakes {
			units = append(units, u.sub.units...)
		} else {
			units = append(units, u)
		}
	}

	s.units = units

	ordered, err := l.ordered(s)
	if err != nil {
		return err
	}

	s.units = l.bound(s, ordered, budget)
	s.takes, s.emits = l.ports(s.nodes)

	return nil
}

// ordered is s's units in dependency order, each after every unit whose
// channels it reads, and otherwise in node order.
func (l *layout) ordered(s *scope) ([]*unit, error) {
	at := map[int]int{}

	for x, u := range s.units {
		for _, i := range unitNodes(u) {
			at[i] = x
		}
	}

	deps := make([]map[int]bool, len(s.units))

	for x, u := range s.units {
		deps[x] = map[int]bool{}

		for _, i := range unitNodes(u) {
			for _, c := range l.nodes[i].uses {
				if o, ok := l.owner[c]; ok {
					if y, in := at[o]; in && y != x {
						deps[x][y] = true
					}
				}
			}
		}
	}

	out := make([]*unit, 0, len(s.units))
	done := map[int]bool{}

	for len(out) < len(s.units) {
		next := -1

		for x := range s.units {
			if !done[x] && subset(deps[x], done) {
				next = x

				break
			}
		}

		if next < 0 {
			return nil, fmt.Errorf("emit %s: %w", s.fqid, errCallCycle)
		}

		done[next] = true
		out = append(out, s.units[next])
	}

	return out, nil
}

func subset(a, b map[int]bool) bool {
	for x := range a {
		if !b[x] {
			return false
		}
	}

	return true
}

func unitNodes(u *unit) []int {
	if u.sub != nil {
		return u.sub.nodes
	}

	return []int{u.node}
}

// statements is what u's statements weigh in the workflow that calls it: a
// node's wiring, or a sub-workflow's call.
func (l *layout) statements(u *unit) int {
	if u.sub != nil {
		return linesWeight(u.sub.call())
	}

	return linesWeight(l.nodes[u.node].lines)
}

// measure is the weight of a workflow of units, their statements, its
// published statement and its takes and emits, and how many channels it
// takes.
func (l *layout) measure(units []*unit) (int, int) {
	published := l.published(units)

	w := linesWeight(published)
	for _, u := range units {
		w += l.statements(u)
	}

	takes, emits := l.ports(runNodes(units))
	ports := len(takes) + len(emits) + len(published)

	return w + portCost*ports, len(takes)
}

// published is the statement that mixes the outs bundles units publish,
// maxTakes sources at a time: a node's, if the pipeline outs read it, and
// a sub-workflow's published channel, if it has one. Its weight does not
// depend on the sub-workflows' names, which a scope is given only once
// every scope is laid out.
func (l *layout) published(units []*unit) []string {
	var srcs []string

	for _, u := range units {
		if u.sub == nil {
			if src, ok := l.pubs[u.node]; ok {
				srcs = append(srcs, src)
			}
		} else if slices.ContainsFunc(u.sub.nodes, func(i int) bool {
			_, ok := l.pubs[i]

			return ok
		}) {
			srcs = append(srcs, u.sub.name+".out."+publishedChannel)
		}
	}

	if len(srcs) == 0 {
		return nil
	}

	var mixes strings.Builder
	for group := range slices.Chunk(srcs, maxTakes) {
		mixes.WriteString(".mix(" + strings.Join(group, ", ") + ")")
	}

	return []string{publishedChannel + " = channel.empty()" + mixes.String()}
}

// publish sets the published statement of s and every scope below it.
func (l *layout) publish(s *scope) {
	s.published = l.published(s.units)

	for _, u := range s.units {
		if u.sub != nil {
			l.publish(u.sub)
		}
	}
}

// bound groups units, while they weigh more than budget, into runs, each
// a workflow that weighs at most maxWeight and takes at most maxTakes
// channels. Each round leaves fewer units, until no two fit one run.
func (l *layout) bound(s *scope, units []*unit, budget int) []*unit {
	for {
		if w, _ := l.measure(units); w <= budget {
			return units
		}

		runs := l.runs(s, units)
		if len(runs) == len(units) {
			return units
		}

		units = runs
	}
}

// runs groups units, in order, into runs: a unit joins the run before it
// while the run fits one workflow.
func (l *layout) runs(s *scope, units []*unit) []*unit {
	out := make([]*unit, 0, len(units))

	for start := 0; start < len(units); {
		end := start + 1
		for end < len(units) && l.fits(units[start:end+1]) {
			end++
		}

		if end-start == 1 {
			out = append(out, units[start])
		} else {
			out = append(out, &unit{sub: l.run(s, units[start:end])})
		}

		start = end
	}

	return out
}

// fits reports whether units fit one workflow.
func (l *layout) fits(units []*unit) bool {
	w, takes := l.measure(units)

	return w <= maxWeight && takes <= maxTakes
}

func runNodes(units []*unit) []int {
	nodes := make([]int, 0, len(units))
	for _, u := range units {
		nodes = append(nodes, unitNodes(u)...)
	}

	slices.Sort(nodes)

	return nodes
}

// run is the scope of a run of s's units.
func (l *layout) run(s *scope, units []*unit) *scope {
	r := &scope{fqid: s.fqid, part: true, units: slices.Clone(units), nodes: runNodes(units)}
	r.takes, r.emits = l.ports(r.nodes)

	return r
}

// ports are what a workflow of nodes takes and emits, in the order their
// channels are defined.
func (l *layout) ports(nodes []int) ([]string, []string) {
	in := map[int]bool{}
	for _, i := range nodes {
		in[i] = true
	}

	defs, uses := map[string]bool{}, map[string]bool{}

	for _, i := range nodes {
		for _, d := range l.nodes[i].defs {
			defs[d] = true
		}

		for _, u := range l.nodes[i].uses {
			uses[u] = true
		}
	}

	var takes, emits []string

	for c := range uses {
		if !defs[c] {
			takes = append(takes, c)
		}
	}

	for c := range defs {
		if slices.ContainsFunc(l.users[c], func(i int) bool { return !in[i] }) {
			emits = append(emits, c)
		}
	}

	byOrder := func(a, b string) int { return l.order[a] - l.order[b] }
	slices.SortFunc(takes, byOrder)
	slices.SortFunc(emits, byOrder)

	return takes, emits
}

// name names every scope below s, in the order main.nf calls them: W<j>
// and its pipeline's call, as a node's alias names its stage call.
func (l *layout) name(s *scope) {
	for _, u := range s.units {
		if u.sub == nil {
			continue
		}

		u.sub.name = "W" + strconv.Itoa(l.names) + "_" + u.sub.fqid[strings.LastIndex(u.sub.fqid, ".")+1:]
		l.names++

		l.name(u.sub)
	}
}
