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
const wiringWorkflows = "ENTRY_ROOT ; FORK ; ELEMENTS ; ZIP ; AFTER ; GATE ; ALIASES ; ALIASED ; GATES ; GATED ; REGATE ; ENABLED ; READ ; REKEY ; GATHER ; NEEDS ; INPUT ; INPUT_ENTRY ; SPLITS ; CHUNKS ; JOIN ; SOURCES ; JOIN_SOURCES ; DONE ; POSTED ; PUBLISHED"

// subworkflowDir holds a module per sub-workflow main.nf calls.
const subworkflowDir = "subworkflows"

// workflows renders main.nf, which runs PLAN, the nodes and OUTS and
// publishes OUTS's outputs, and a module per sub-workflow below
// subworkflowDir.
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

	root := p.scopes(nodes, plan, outs)
	files := map[string]string{}
	addModules(files, root, nodes)

	var b strings.Builder

	b.WriteString("include { shq ; code ; ordered ; bundlesOf ; refBundles ; skeletons ; bundleLocations ; recordsOf ; published ; callStaged ; pool ; OUTS ; CALL ; NODES ; ENTRY ; ENTRY_FORKS ; " + wiringWorkflows + " } from './stage.nf'\n")
	writeIncludes(&b, root, nodes, "./stage.nf", "./"+subworkflowDir+"/")
	b.WriteString("\n")
	b.WriteString(p.paramsBlock())
	b.WriteString(planProcess)
	b.WriteString("workflow {\n    main:\n")
	writeLines(&b, plan.lines)
	writeUnits(&b, root, nodes)
	writeLines(&b, outs.lines)
	b.WriteString(publication)

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
		fmt.Fprintf(&b, "\nworkflow %s {\n", u.sub.name)

		if len(u.sub.takes) > 0 {
			b.WriteString("    take:\n")
			writeLines(&b, u.sub.takes)
			b.WriteString("\n")
		}

		b.WriteString("    main:\n")
		writeUnits(&b, u.sub, nodes)

		emits := u.sub.emits
		if len(u.sub.published) > 0 {
			emits = append(slices.Clone(emits), publishedChannel)
		}

		if len(emits) > 0 {
			b.WriteString("\n    emit:\n")
			writeLines(&b, emits)
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

// writeUnits writes the statements of s's units, a node's wiring or a
// sub-workflow's call and the channels it emits, and s's published
// statement.
func writeUnits(b *strings.Builder, s *scope, nodes []*wiring) {
	for _, u := range s.units {
		if u.sub == nil {
			writeLines(b, nodes[u.node].lines)

			continue
		}

		writeLines(b, u.sub.call())
	}

	writeLines(b, s.published)
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
// every task shares, with the launch params their scripts read (stageRun),
// the pipelines' version every job records and the run's call, inline when
// its base64 is at most cg.MaxValueBytes, as an output value is, or else
// staged (CALL), one value, as combine makes a queue of two values;
// nodes maps each witness node to its digest, its off flag, its statically
// disabled fork keys, the cpus and memory its first task is scheduled with
// and its own overrides, paired with shared, which a node then reads from
// no channel of its own; entry maps each node and fork of its entry-fed
// roots to the entry files staged there, and each of those to its file(),
// which globs no name; entry_forks maps each entry-fed root to its forks,
// so the wiring is the same few lines however many roots there are.
func (p *project) planWiring(ws []*wiring) *wiring {
	used := map[string]bool{}

	for _, w := range ws {
		for _, u := range w.uses {
			used[u] = true
		}
	}

	w := &wiring{}
	w.linef("if (params.mro2nf_require_image && !params.mro2nf_image) {")
	w.linef("    error 'a container profile runs every task in params.mro2nf_image: pass --mro2nf_image <image>, an image built FROM the mro2nf runtime image that holds the pipeline code at the paths it was transpiled from'")
	w.linef("}")
	w.linef("PLAN(%s, %s, workflow.workDir.toUriString(), file(\"${projectDir}/%s\"), params.mro2nf_overrides ? file(params.mro2nf_overrides, checkIfExists: true) : [])", p.values(), p.graphMeta(planRun), mroAssets)
	w.linef("%s = CALL(PLAN.out.call, %d).combine(PLAN.out.pipelines).map { c, v -> %s }.first()", w.def("shared"), cg.MaxValueBytes, p.graphMeta("call: c", "pipelines: v", stageRun))

	for _, d := range []struct{ name, expr string }{
		{"nodes", "NODES(PLAN.out.nodes, shared)"},
		{"entry", "ENTRY(PLAN.out.files, PLAN.out.leaves)"},
		{entryForks, "ENTRY_FORKS(PLAN.out.roots)"},
	} {
		if used[d.name] {
			w.linef("%s = %s", w.def(d.name), d.expr)
		}
	}

	return w
}

// planRun and stageRun are the run entry of PLAN's meta and of the meta
// every stage task and OUTS share: the launch params their scripts read,
// as an input, which Nextflow keys a -resume on. Nextflow 26.04 keys no
// task on a param read by a function its script calls, as mreStage, and
// 26.04.0-26.04.1 on none its script reads (nextflow-io/nextflow#7156), so
// a changed param would silently reuse a stale task. A boolean is the Boolean the params block
// types it as, so a launch's --<name> false keys as its default false does.
// A stage task's uuid is the run's pipestance uuid its jobs get as
// MRO_UUID: the launch environment's MRO_FORCE_UUID, as mrp honours it
// (core/runtime.go:446), or else the Nextflow session id, which a -resume
// keeps, as mrp's restart keeps the pipestance's uuid, and which keys
// every task already. Its workdir is the run's work dir, whose tasks' files
// lie under no read-only root, though an include path may hold it.
const (
	planRun  = "run: [mre: params.mro2nf_mre, psid: params.mro2nf_psid, host_inputs: params.mro2nf_host_inputs, host_in_place: params.mro2nf_host_in_place, remote_inputs: params.mro2nf_allow_remote_inputs, s3_mount: params.mro2nf_s3_mount, local_jobs: params.mro2nf_local_jobs]"
	stageRun = "run: [mre: params.mro2nf_mre, psid: params.mro2nf_psid, uuid: env('MRO_FORCE_UUID') ?: workflow.sessionId.toString(), mrjob: params.mro2nf_mrjob, adapters: params.mro2nf_adapters, monitor: params.mro2nf_monitor, confine: params.mro2nf_confine, s3_mount: params.mro2nf_s3_mount, host_in_place: params.mro2nf_host_in_place, local_jobs: params.mro2nf_local_jobs, workdir: workflow.workDir.toUriString()]"
)

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
