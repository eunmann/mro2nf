package cgnf

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/eunmann/mro2nf/internal/cg"
)

// gen accumulates main.nf: the module includes, the processes main.nf
// declares itself, and the workflow body.
type gen struct {
	incs, procs, wf strings.Builder
}

func (g *gen) linef(format string, args ...any) {
	fmt.Fprintf(&g.wf, "  "+format+"\n", args...)
}

// mainNF renders main.nf: PLAN, every node's instances, gates, staged
// inputs and outputs in dependency order, then OUTS.
func (p *project) mainNF() (string, error) {
	g := &gen{}

	g.incs.WriteString("include { shq ; ordered ; jsonOf ; sized ; OUTS } from './stage.nf'\n")
	p.planProcess(g)
	p.planWiring(g)

	for i, n := range p.topo.Nodes {
		if err := p.node(g, i, n); err != nil {
			return "", err
		}
	}

	p.outs(g)

	return g.incs.String() + "\n" + g.procs.String() + "workflow {\n" + g.wf.String() + "}\n", nil
}

// alias is node i's process alias.
func alias(i int, fqid string) string {
	return "N" + strconv.Itoa(i) + "_" + fqid[strings.LastIndex(fqid, ".")+1:]
}

// gstr is s as a Groovy single-quoted string literal.
func gstr(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}

func glist(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = gstr(s)
	}

	return "[" + strings.Join(q, ", ") + "]"
}

// param names a closure parameter, underscored when unused so the closure
// lints clean.
func param(name string, used bool) string {
	if used {
		return name
	}

	return "_" + name
}

// pick lists v[i] for each i.
func pick(v string, idx []int) string {
	parts := make([]string, len(idx))
	for j, i := range idx {
		parts[j] = v + "[" + strconv.Itoa(i) + "]"
	}

	return "[" + strings.Join(parts, ", ") + "]"
}

// planWiring parses PLAN's records: nodes maps each witness node to its
// digest, its off flag and its statically disabled fork keys; f<j> are the
// forks of entry-fed root j, each [outer key, own ordinal, record].
func (p *project) planWiring(g *gen) {
	g.linef("plan = \"${workflow.workDir}/mro2nf-plan/${workflow.sessionId}\".toString()")
	g.linef("PLAN(%s, %s, file(\"${projectDir}/_assets/source.json\"))", p.values(), p.graphMeta("plan"))
	g.linef("nodes = PLAN.out.nodes.map { s -> s.readLines().collectEntries { l -> def f = l.tokenize(' '); [(f[0]): f.drop(1)] } }")

	for j := range p.entry {
		g.linef("f%d = PLAN.out.r%d.flatMap { s -> s.readLines().collect { l -> def f = l.tokenize(' '); def k = f[0].tokenize('.').collect { t -> t as int }; [k.take(k.size() - 1), k[-1], f[1]] } }", j, j)
	}
}

// graphMeta is the meta map naming the run's invocation, with extra
// entries.
func (p *project) graphMeta(plan string, extra ...string) string {
	entries := append([]string{
		"mro: " + gstr(p.src.Name),
		"mropath: " + gstr(strings.Join(p.opts.MroPaths, ":")),
		"plan: " + plan,
		"build: " + gstr(p.opts.Version),
	}, extra...)

	return "[" + strings.Join(entries, ", ") + "]"
}

// nodeMeta is node n's meta: its identity and the resources it is
// scheduled with.
func (p *project) nodeMeta(n cg.TopoNode) (string, error) {
	s, err := p.stage(n.Fqid)
	if err != nil {
		return "", err
	}

	var threads, memGB float64

	special := ""

	if r := s.Resources; r != nil {
		threads, memGB, special = float64(r.Threads), float64(r.MemGB), r.Special
	}

	cpus := int(math.Max(1, math.Ceil(math.Abs(threads))))

	mem := math.Abs(memGB)
	if mem == 0 {
		mem = 1
	}

	return p.graphMeta("plan",
		"node: "+gstr(n.Fqid),
		"cpus: "+strconv.Itoa(cpus),
		"mem_gb: "+strconv.FormatFloat(mem, 'f', -1, 64),
		"special: "+gstr(special),
		"digest: d",
	), nil
}

// gpus is the GPU count `special = "gpu[:N]"` requests, else 0.
func gpus(special string) int {
	if special == "gpu" {
		return 1
	}

	if rest, ok := strings.CutPrefix(special, "gpu:"); ok {
		if n, err := strconv.Atoi(rest); err == nil && n > 0 {
			return n
		}
	}

	return 0
}

// consumed reports which of node fqid's outputs a later node or the
// pipeline outs read: its outs, its fork elements, its gates.
func (p *project) consumed(fqid string) (bool, bool, bool) {
	outs := slices.ContainsFunc(p.topo.Outs, func(r cg.Read) bool { return r.Node == fqid })
	gates := false

	for _, n := range p.topo.Nodes {
		outs = outs || slices.ContainsFunc(n.Reads, func(r cg.Read) bool { return r.Node == fqid })
		gates = gates || slices.ContainsFunc(n.Gates, func(g cg.Gate) bool { return g.Node == fqid })
	}

	forks := slices.ContainsFunc(p.topo.Roots, func(r cg.TopoRoot) bool { return r.Producer == fqid })

	return outs, forks, gates
}
