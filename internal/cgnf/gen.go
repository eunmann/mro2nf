package cgnf

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/eunmann/mro2nf/internal/cg"
)

// wiringWorkflows are the stage module's node wiring sub-workflows, which
// every workflow of the project includes.
const wiringWorkflows = "FORK ; ELEMENTS ; ZIP ; AFTER ; GATE ; GATED ; ENABLED ; SELECT ; READ ; GATHER ; GATHER_SIZED ; COUNTS ; INPUT ; INPUT_ENTRY ; CHUNKS ; JOIN ; DONE ; OR_SKIPPED"

// subworkflowDir holds a module per sub-workflow main.nf calls.
const subworkflowDir = "subworkflows"

// workflows renders main.nf, which runs PLAN, the root pipeline's calls and
// OUTS, and a module per sub-workflow below subworkflowDir.
func (p *project) workflows() (map[string]string, error) {
	nodes := make([]*wiring, len(p.topo.Nodes))

	for i, n := range p.topo.Nodes {
		w, err := p.node(i, n)
		if err != nil {
			return nil, err
		}

		nodes[i] = w
	}

	outs := p.outs()
	plan := p.planWiring(append(slices.Clone(nodes), outs))

	root, err := p.scopes(nodes, plan, outs)
	if err != nil {
		return nil, err
	}

	files := map[string]string{}
	addModules(files, root, nodes)

	var b strings.Builder

	b.WriteString("include { shq ; code ; ordered ; on ; OUTS ; NODES ; ENTRY ; ENTRY_FORKS ; ENTRY_COUNTS ; PUBLISHED ; " + wiringWorkflows + " } from './stage.nf'\n")
	writeIncludes(&b, root, nodes, "./stage.nf", "./"+subworkflowDir+"/")
	b.WriteString("\n")
	p.planProcess(&b)
	b.WriteString("workflow {\n")
	writeLines(&b, plan.lines)
	writeUnits(&b, root, nodes)
	writeLines(&b, outs.lines)
	b.WriteString("}\n")

	files["main.nf"] = b.String()

	return files, nil
}

// addModules adds to files the module of every sub-workflow below s.
func addModules(files map[string]string, s *scope, nodes []*wiring) {
	for _, u := range s.units {
		if u.sub == nil {
			continue
		}

		addModules(files, u.sub, nodes)

		var b strings.Builder

		b.WriteString("include { " + wiringWorkflows + " } from '../stage.nf'\n")
		writeIncludes(&b, u.sub, nodes, "../stage.nf", "./")
		fmt.Fprintf(&b, "\n// %s\nworkflow %s {\n", u.sub.label(), u.sub.name)

		if len(u.sub.takes) > 0 {
			b.WriteString("    take:\n")
			writeLines(&b, u.sub.takes)
			b.WriteString("\n")
		}

		b.WriteString("    main:\n")
		writeUnits(&b, u.sub, nodes)

		if len(u.sub.emits) > 0 {
			b.WriteString("\n    emit:\n")
			writeLines(&b, u.sub.emits)
		}

		b.WriteString("}\n")

		files[subworkflowDir+"/"+u.sub.name+".nf"] = b.String()
	}
}

// writeIncludes writes the includes of the processes and sub-workflows s's
// own units call.
func writeIncludes(b *strings.Builder, s *scope, nodes []*wiring, stage, subs string) {
	for _, u := range s.units {
		if u.sub != nil {
			fmt.Fprintf(b, "include { %s } from '%s%s.nf'\n", u.sub.name, subs, u.sub.name)

			continue
		}

		for _, inc := range nodes[u.node].incs {
			fmt.Fprintf(b, "include { %s } from '%s'\n", inc, stage)
		}
	}
}

// writeUnits writes the statements of s's units: a node's wiring, or a
// sub-workflow's call and the channels it emits.
func writeUnits(b *strings.Builder, s *scope, nodes []*wiring) {
	for _, u := range s.units {
		if u.sub == nil {
			writeLines(b, nodes[u.node].lines)

			continue
		}

		writeLines(b, u.sub.call())
	}
}

func writeLines(b *strings.Builder, lines []string) {
	for _, l := range lines {
		b.WriteString("    " + l + "\n")
	}
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

func ilist(xs []int) string {
	q := make([]string, len(xs))
	for i, x := range xs {
		q[i] = strconv.Itoa(x)
	}

	return "[" + strings.Join(q, ", ") + "]"
}

// planWiring parses PLAN's records the wirings read: shared is the meta
// every task shares, with the run's call; nodes maps each witness node to
// its digest, its off flag, its statically disabled fork keys, the cpus and
// memory its first task is scheduled with and its own overrides; entry
// maps each node and fork of its entry-fed roots to the entry files staged
// there, and each of those to its file(), which globs no name; f<j> are the
// forks of entry-fed root j and n<j> their counts under each fork of its
// outer roots.
func (p *project) planWiring(ws []*wiring) *wiring {
	used := map[string]bool{}

	for _, w := range ws {
		for _, u := range w.uses {
			used[u] = true
		}
	}

	w := &wiring{}
	w.linef("if (on(params.mro2nf_require_image) && !params.mro2nf_image) {")
	w.linef("    error 'a container profile runs every task in params.mro2nf_image: pass --mro2nf_image <image>, an image built FROM the mro2nf runtime image that holds the pipeline code at the paths it was transpiled from'")
	w.linef("}")
	w.linef("PLAN(%s, %s, file(\"${projectDir}/%s\"), params.mro2nf_overrides ? file(params.mro2nf_overrides, checkIfExists: true) : [])", p.values(), p.graphMeta(), mroAssets)
	w.linef("%s = PLAN.out.call.map { c -> %s }", w.def("shared"), p.graphMeta("call: c"))

	for _, d := range []struct{ name, expr string }{
		{"nodes", "NODES(PLAN.out.nodes)"},
		{"entry", "ENTRY(PLAN.out.files, PLAN.out.leaves)"},
	} {
		if used[d.name] {
			w.linef("%s = %s", w.def(d.name), d.expr)
		}
	}

	for j := range p.entry {
		if f := entryForksOf(j); used[f] {
			w.linef("%s = ENTRY_FORKS(PLAN.out.r%d)", w.def(f), j)
		}

		if n := entryCountsOf(j); used[n] {
			w.linef("%s = ENTRY_COUNTS(PLAN.out.n%d)", w.def(n), j)
		}
	}

	return w
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

// nodeMeta is node n's own meta: its identity and the special resource
// it is scheduled with; PLAN's record of the node gives its cpus, memory
// and overrides (planned).
func (p *project) nodeMeta(n cg.TopoNode) (string, error) {
	s, err := p.stage(n.Fqid)
	if err != nil {
		return "", err
	}

	special := ""
	if r := s.Resources; r != nil {
		special = r.Special
	}

	return "[node: " + gstr(n.Fqid) + ", special: " + gstr(special) + "]", nil
}

// awaited reports whether a node waits for preflight fqid, which then
// signals, as d<i>, once every instance of it is done.
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
