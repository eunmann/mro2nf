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

	g.incs.WriteString("include { shq ; code ; ordered ; resources ; OUTS } from './stage.nf'\n")
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
// digest, its off flag and its statically disabled fork keys; entry maps
// each node and fork of its entry-fed roots to the entry files staged
// there, and each of those to its file(), which globs no name; f<j> are the
// forks of entry-fed root j, each [outer key, own ordinal, record, index
// token].
func (p *project) planWiring(g *gen) {
	g.linef("if (params.mro2nf_code && !params.mro2nf_image) {")
	g.linef("  error 'a container profile runs every stage task in params.mro2nf_image: build it with ./build.sh <tag>, then pass --mro2nf_image <tag>'")
	g.linef("}")
	g.linef("if (params.mro2nf_code && !params.mro2nf_runtime_image) {")
	g.linef("  error 'a container profile runs PLAN and OUTS in params.mro2nf_runtime_image, which no release published for this transpiler build: build it with docker build -f deploy/runtime/Dockerfile -t <image> . in an mro2nf checkout, then pass --mro2nf_runtime_image <image>'")
	g.linef("}")
	g.linef("PLAN(%s, %s, file(\"${projectDir}/%s\"))", p.values(), p.graphMeta(), mroAssets)
	g.linef("call = PLAN.out.call")

	if len(p.topo.Nodes) > 0 {
		g.linef("nodes = PLAN.out.nodes.map { s -> s.readLines().collectEntries { l -> def f = l.tokenize(' '); [(f[0]): f.drop(1)] } }")
	}

	if len(p.readers) > 0 {
		g.linef("entry = PLAN.out.files.combine(PLAN.out.leaves).map { fl, ll -> [files: fl.readLines().collectEntries { l -> def f = l.tokenize(' '); [(f[0] + ' ' + f[1]): f[2].tokenize(',')] }, leaves: ll.readLines().collectEntries { l -> def f = l.tokenize(' '); [(f[0]): file(new String(f[1].decodeBase64(), 'UTF-8'), glob: false)] }] }")
	}

	for j := range p.entry {
		g.linef("f%d = PLAN.out.r%d.flatMap { s -> s.readLines().collect { l -> def f = l.tokenize(' '); def k = f[0].tokenize('.').collect { t -> t as int }; [k.take(k.size() - 1), k[-1], f[1], f[2]] } }", j, j)
	}
}

// graphMeta is the meta map naming the entry .mro and the include path the
// run's invocation compiles with, with extra entries.
func (p *project) graphMeta(extra ...string) string {
	entries := append([]string{
		"entry: " + gstr(p.src.Path()),
		"mropath: " + gstr(strings.Join(p.opts.MroPaths, ":")),
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

	return p.graphMeta(
		"call: c",
		"node: "+gstr(n.Fqid),
		"cpus: "+strconv.Itoa(cpus),
		"mem_gb: "+strconv.FormatFloat(mem, 'f', -1, 64),
		"special: "+gstr(special),
		"digest: d",
	), nil
}

// awaited reports whether a node waits for preflight fqid, which then
// signals, as d<i>, once every instance of it is done: its tasks have all
// completed, and a disabled instance runs none.
func (p *project) awaited(fqid string) bool {
	return slices.ContainsFunc(p.topo.Nodes, func(n cg.TopoNode) bool { return slices.Contains(n.Preflights, fqid) })
}

// consumed reports which of node fqid's outputs a later node or the
// pipeline outs read: its outs, its fork and partner elements, its gates.
func (p *project) consumed(fqid string) (bool, bool, bool) {
	outs := slices.ContainsFunc(p.topo.Outs, func(r cg.Read) bool { return r.Node == fqid })
	gates := false

	for _, n := range p.topo.Nodes {
		outs = outs || slices.ContainsFunc(n.Reads, func(r cg.Read) bool { return r.Node == fqid })
		gates = gates || slices.ContainsFunc(n.Gates, func(g cg.Gate) bool { return g.Node == fqid })
	}

	forks := slices.ContainsFunc(p.topo.Roots, func(r cg.TopoRoot) bool { return r.Producer == fqid })

	for _, n := range p.topo.Nodes {
		forks = forks || slices.ContainsFunc(n.Zips, func(z cg.Zip) bool { return z.Node == fqid })
	}

	return outs, forks, gates
}
