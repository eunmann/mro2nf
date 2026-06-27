package emit

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/types"
)

// genCtx carries the resolved paths and names needed to render Nextflow code.
type genCtx struct {
	entry   string
	mroFile string
	mre     string
	shell   string
	mrjob   string
	monitor bool
	code    map[string]string // stage name -> stage code path
}

// stageCmd renders an mre invocation for a stage phase, single-quoting every
// path so spaces and shell metacharacters in paths are safe.
func (g genCtx) stageCmd(phase, code string, lang ir.Lang) string {
	cmd := fmt.Sprintf("'%s' %s -shell '%s' -stagecode '%s' -lang %s -call '%s' -mro '%s'",
		g.mre, phase, g.shell, code, lang, g.entry, g.mroFile)

	if g.mrjob != "" {
		cmd += fmt.Sprintf(" -mrjob '%s'", g.mrjob)
	}

	if g.monitor {
		cmd += " -monitor"
	}

	return cmd
}

// producerArgs renders the flags a bundle-producing mre command needs to stage
// the file leaves of callable's params under the given role.
func (g genCtx) producerArgs(callable, role string) string {
	return fmt.Sprintf(" -types 'types.json' -callable '%s' -role %s", callable, role)
}

// generateStageModule renders a stage's module: its processes and the wf_<stage>
// subworkflow that wraps them.
func generateStageModule(s *ir.Stage, g genCtx) string {
	var b strings.Builder

	genStage(&b, s, g)

	return b.String()
}

// generatePipeModule renders a pipeline's module: includes of its callees (each
// aliased per call so repeated calls get independent instances), the per-call
// helper processes, and the pipeline workflow.
func generatePipeModule(p *ir.Pipeline, prog *ir.Program, g genCtx) string {
	var b strings.Builder

	genPipeIncludes(&b, p, prog)
	genKeyedPipeIncludes(&b, p, prog)
	genPipeProcesses(&b, p, prog, g)
	genKeyedPipeProcesses(&b, p, prog, g)
	genPipelineWorkflow(&b, p)
	genKeyedPipeline(&b, p)

	return b.String()
}

// generateMain renders main.nf: the entry workflow plus PUBLISH. The entry
// callable may be a pipeline or a bare stage.
func generateMain(prog *ir.Program, g genCtx) string {
	var b strings.Builder

	export, src := calleeModule(prog, prog.Entry.Callable)

	fmt.Fprintf(&b, "include { %s } from './modules/%s'\n\n", export, strings.TrimPrefix(src, "./"))
	genPublish(&b, prog.Entry.Callable, g)
	genEntry(&b, prog, export, g)

	return b.String()
}

// genPipeIncludes emits one include per call, aliasing the callee's workflow to
// a per-call name so each call is an independent instance.
func genPipeIncludes(b *strings.Builder, p *ir.Pipeline, prog *ir.Program) {
	for _, c := range p.Calls {
		export, src := calleeExport(prog, c)
		fmt.Fprintf(b, "include { %s as %s } from '%s'\n", export, callAlias(p.Name, c.Name), src)
	}

	b.WriteString("\n")
}

// calleeExport returns the workflow name and module to import for a call. A map
// call over a split stage imports that stage's fork-key-threaded variant
// (wf_<stage>_map) instead of the plain wf_<stage>.
func calleeExport(prog *ir.Program, c ir.Call) (string, string) {
	export, src := calleeModule(prog, c.Callable)
	if c.Mapped {
		// Every map target runs through its fork-key-threaded variant.
		export = "wf_" + c.Callable + "_map"
	}

	return export, src
}

// keyedCallAlias is the per-call alias under which a callee's fork-key-threaded
// variant (wf_<callable>_map) is imported into a keyed pipeline.
func keyedCallAlias(pipeline, call string) string {
	return "wfk_" + qualify(pipeline, call)
}

// genKeyedPipeIncludes imports the fork-key-threaded variant of each callee so a
// keyed pipeline can run its body per fork.
func genKeyedPipeIncludes(b *strings.Builder, p *ir.Pipeline, prog *ir.Program) {
	for _, c := range p.Calls {
		_, src := calleeModule(prog, c.Callable)
		fmt.Fprintf(b, "include { wf_%s_map as %s } from '%s'\n", c.Callable, keyedCallAlias(p.Name, c.Name), src)
	}

	b.WriteString("\n")
}

// genKeyedPipeProcesses emits the fork-key-carrying bind processes a keyed
// pipeline uses (one per call, plus the return).
func genKeyedPipeProcesses(b *strings.Builder, p *ir.Pipeline, prog *ir.Program, g genCtx) {
	for _, c := range p.Calls {
		if c.Mapped {
			genKeyedForkBindProcess(b, p.Name, c, g)
			genKeyedMergeProcess(b, p.Name, c, calleeOutNames(prog, c.Callable), g)

			if c.Disabled != nil {
				genKeyedBindProcess(b, disableName(p.Name, c.Name), disableBindings(c), g, "", "disable")
			}

			continue
		}

		genKeyedBindProcess(b, bindName(p.Name, c.Name), c.Bindings, g, g.producerArgs(c.Callable, types.RoleIn), "args")

		if c.Disabled != nil {
			genKeyedBindProcess(b, disableName(p.Name, c.Name), disableBindings(c), g, "", "disable")
		}
	}

	genKeyedBindProcess(b, bindName(p.Name, "return"), p.Returns, g, g.producerArgs(p.Name, types.RoleOut), `outs__${key}`)
}

// genKeyedBindProcess emits a fork-key-carrying bind: it joins the fork's
// pipeline args and referenced upstream outputs by key, then resolves them per
// fork. out is the produced bundle's name ("args" for an intermediate call,
// "outs__${key}" for the return so the merge can order forks).
func genKeyedBindProcess(b *strings.Builder, name string, bindings []ir.Binding, g genCtx, prodArgs, out string) {
	block, arg := keyedInputs(refCalls(bindings))

	fmt.Fprintf(b, `process %[1]s_K {
  input:
%[2]s  output:
    tuple val(key), path("%[6]s", type: 'dir')
  script:
    """
    '%[3]s' bind -spec 'spec.json' -pipeargs ${pipeargs}%[4]s -o %[6]s%[5]s
    """
}

`, name, block, g.mre, arg, prodArgs, out)
}

// keyedInputs renders a keyed process's input block — tuple(key, pipeargs, staged
// upstream bundles) — and the matching `-inputs id=in_id` argument.
func keyedInputs(refs []string) (string, string) {
	var in strings.Builder

	in.WriteString("    tuple val(key), path(pipeargs)")

	pairs := make([]string, 0, len(refs))
	for _, id := range refs {
		fmt.Fprintf(&in, ", path('in_%s')", id)
		pairs = append(pairs, fmt.Sprintf("%s=in_%s", id, id))
	}

	in.WriteString("\n")
	// The shared type manifest and this process's own bindspec are staged as two
	// individual files (not the whole _assets dir), so a task transfers only the
	// one bindspec it needs rather than every call's spec; see assetsDir.
	in.WriteString("    path 'types.json'\n")
	in.WriteString("    path 'spec.json'\n")

	arg := ""
	if len(pairs) > 0 {
		arg = " -inputs " + strings.Join(pairs, ",")
	}

	return in.String(), arg
}

// genKeyedForkBindProcess emits the fork-key-threaded forkbind for a nested map
// call: per outer fork it forks the split collection into inner fork bundles.
func genKeyedForkBindProcess(b *strings.Builder, pipeline string, c ir.Call, g genCtx) {
	block, arg := keyedInputs(refCalls(c.Bindings))

	// The forks are emitted as a single directory (a tuple-glob path captures
	// only one match, unlike a list output); the workflow lists it per fork.
	fmt.Fprintf(b, `process %[1]s_K {
  input:
%[2]s  output:
    tuple val(key), path('forks'), emit: forks
    tuple val(key), path('forkkeys.json'), emit: keys
  script:
    """
    mkdir -p forks
    '%[3]s' forkbind -spec 'spec.json' -pipeargs ${pipeargs}%[4]s -chunkdir forks%[5]s
    mv -f forks/forkkeys.json forkkeys.json
    """
}

`, forkName(pipeline, c.Name), block, g.mre, arg, g.producerArgs(c.Callable, types.RoleIn))
}

// genKeyedMergeProcess emits the fork-key-threaded merge for a nested map call:
// per outer fork it gathers that fork's inner results.
func genKeyedMergeProcess(b *strings.Builder, pipeline string, c ir.Call, calleeOuts string, g genCtx) {
	fmt.Fprintf(b, `process %[1]s_K {
  input:
    tuple val(key), path(souts), path('forkkeys.json')
    path 'types.json'
  output:
    tuple val(key), path('merged', type: 'dir')
  script:
    """
    '%[2]s' merge -outs '%[3]s' -files "\$(ls -1d outs__* 2>/dev/null | sort -V | paste -sd, -)" -keys-file forkkeys.json -o merged%[4]s
    """
}

`, mergeName(pipeline, c.Name), g.mre, calleeOuts, g.producerArgs(c.Callable, types.RoleOut))
}

// genKeyedPipeline emits wf_<pipeline>_map: the pipeline body run once per fork,
// with the fork key threaded through every bind and callee. Each channel is
// collapsed to a value list (toList) so it can be re-read by multiple consumers
// without exhausting the fork queue; binds join their inputs by key.
func genKeyedPipeline(b *strings.Builder, p *ir.Pipeline) {
	var body strings.Builder

	body.WriteString("  main:\n    pa_l = keyed.toList()\n")
	body.WriteString("    types = file(\"${projectDir}/_assets/types.json\")\n")

	for _, c := range p.Calls {
		genKeyedCallBody(&body, p.Name, c)
	}

	retName := bindName(p.Name, "return")
	fmt.Fprintf(&body, "    %s_K(%s)\n", retName, keyedBindCall(p.Returns, retName))

	fmt.Fprintf(b, `workflow wf_%s_map {
  take: keyed
%s  emit:
    %s_K.out
}

`, p.Name, body.String(), retName)
}

// genKeyedCallBody emits one call's wiring inside a keyed pipeline: a keyed bind
// then the callee's _map variant, threading the fork key. A disabled call is
// gated per fork — its keyed bind and a keyed DISABLE are joined by key and
// branched, running the callee only for enabled forks and emitting the null
// bundle for skipped ones.
func genKeyedCallBody(body *strings.Builder, pipeline string, c ir.Call) {
	if c.Mapped {
		genKeyedMappedCallBody(body, pipeline, c)

		return
	}

	bind := bindName(pipeline, c.Name)
	alias := keyedCallAlias(pipeline, c.Name)
	fmt.Fprintf(body, "    %s_K(%s)\n", bind, keyedBindCall(c.Bindings, bind))

	if c.Disabled == nil {
		fmt.Fprintf(body, "    ch_%s_l = %s(%s_K.out).toList()\n", c.Name, alias, bind)

		return
	}

	dis := disableName(pipeline, c.Name)
	nulls := "${projectDir}/nulls/" + qualify(pipeline, c.Name)
	fmt.Fprintf(body, "    %s_K(%s)\n", dis, keyedBindCall(disableBindings(c), dis))
	fmt.Fprintf(body, `    g_%[1]s = %[2]s_K.out.join(%[3]s_K.out).branch { key, args, d ->
        def off = new groovy.json.JsonSlurper().parseText(d.resolve('data.json').text).disabled
        run: !off
        skip: off
    }
    r_%[1]s = %[4]s(g_%[1]s.run.map { key, args, d -> tuple(key, args) })
    s_%[1]s = g_%[1]s.skip.map { key, args, d -> tuple(key, file("%[5]s")) }
    ch_%[1]s_l = r_%[1]s.mix(s_%[1]s).toList()
`, c.Name, bind, dis, alias, nulls)
}

// genKeyedMappedCallBody emits a nested map call inside a keyed pipeline. Per
// outer fork the FORKBIND forks the split collection; each inner fork is given a
// composite "outer~inner" key and run through the callee's _map variant; results
// are regrouped by stripping the innermost key segment (so arbitrary nesting
// works) and merged per outer fork. A remainder join keeps an outer fork whose
// inner collection was empty (it merges to null).
func genKeyedMappedCallBody(body *strings.Builder, pipeline string, c ir.Call) {
	fork := forkName(pipeline, c.Name)
	merge := mergeName(pipeline, c.Name)
	alias := keyedCallAlias(pipeline, c.Name)

	// A disabled nested map is all-or-nothing per outer fork: the disable flag is
	// resolved per key and gates whether that fork's inner map runs at all. The
	// FORKBIND reuses the call's bind spec (see genKeyedForkBindProcess).
	forkInput := keyedBindCall(c.Bindings, bindName(pipeline, c.Name))
	if c.Disabled != nil {
		forkInput = genKeyedMappedDisableGate(body, pipeline, c)
	}

	fmt.Fprintf(body, "    %s_K(%s)\n", fork, forkInput)
	// Enumerate the per-fork bundle dirs from forknames.json rather than a java.io
	// listFiles() call, which cannot list an object-store (s3://) work dir. Reading
	// the staged names file and constructing each fork path is object-store-safe.
	fmt.Fprintf(body, "    ik_%[1]s = %[2]s_K.out.forks.flatMap { ok, d -> new groovy.json.JsonSlurper().parseText(d.resolve('forknames.json').text).collect { fn -> tuple(\"${ok}~${fn}\", d.resolve(fn)) } }\n", c.Name, fork)
	fmt.Fprintf(body, "    io_%[1]s = %[2]s(ik_%[1]s)\n", c.Name, alias)
	fmt.Fprintf(body, "    mj_%[1]s = %[2]s_K.out.keys.join(io_%[1]s.map { ck, bdl -> tuple(ck[0..<ck.lastIndexOf('~')], bdl) }.groupTuple(), remainder: true).map { ok, fk, so -> tuple(ok, so ?: [], fk) }\n", c.Name, fork)
	fmt.Fprintf(body, "    %s_K(mj_%s, types)\n", merge, c.Name)

	if c.Disabled != nil {
		// Skipped outer forks bypass FORKBIND/MERGE entirely; mix their null
		// bundles back in so every outer key has a result.
		fmt.Fprintf(body, "    ch_%[1]s_l = %[2]s_K.out.mix(sk_%[1]s).toList()\n", c.Name, merge)

		return
	}

	fmt.Fprintf(body, "    ch_%[1]s_l = %[2]s_K.out.toList()\n", c.Name, merge)
}

// genKeyedMappedDisableGate emits a keyed DISABLE bind and a per-outer-fork
// run/skip branch for a disabled nested map call. It returns the run-branch
// channel expression (the FORKBIND input with the disable-flag bundle stripped)
// and emits sk_<call>, the keyed null bundles for skipped outer forks.
func genKeyedMappedDisableGate(body *strings.Builder, pipeline string, c ir.Call) string {
	dis := disableName(pipeline, c.Name)
	nulls := "${projectDir}/nulls/" + qualify(pipeline, c.Name)

	fmt.Fprintf(body, "    %s_K(%s)\n", dis, keyedBindCall(disableBindings(c), dis))
	fmt.Fprintf(body, `    gk_%[1]s = %[2]s.join(%[3]s_K.out).branch { row ->
        def off = new groovy.json.JsonSlurper().parseText(row[-1].resolve('data.json').text).disabled
        run: !off
        skip: off
    }
    sk_%[1]s = gk_%[1]s.skip.map { row -> tuple(row[0], file("%[4]s")) }
`, c.Name, keyedBindInput(c.Bindings), dis, nulls)

	return fmt.Sprintf("gk_%s.run.map { row -> row[0..<row.size() - 1] }, types, %s", c.Name, specFile(bindName(pipeline, c.Name)))
}

// keyedBindInput renders the channel expression feeding a keyed bind: the fork
// pipeline args joined by key with each referenced upstream call's output.
func keyedBindInput(bindings []ir.Binding) string {
	var expr strings.Builder

	expr.WriteString("pa_l.flatMap { x -> x }")
	for _, id := range refCalls(bindings) {
		fmt.Fprintf(&expr, ".join(ch_%s_l.flatMap { x -> x })", id)
	}

	return expr.String()
}

// keyedBindCall renders a keyed process invocation argument list: the keyed
// channel expression plus the two final broadcast inputs every bind/fork process
// takes — the shared type manifest and this process's own bindspec (specName).
// Used where keyedBindInput feeds a process call (not where it is a channel
// expression transformed further, e.g. the disable gate's join).
func keyedBindCall(bindings []ir.Binding, specName string) string {
	return keyedBindInput(bindings) + ", types, " + specFile(specName)
}

// specFile is the head-node expression that resolves a call's bindspec so
// Nextflow stages it into the task as `spec.json`. Each bind/fork/disable
// process stages only its own spec rather than every call's (see assetsDir).
func specFile(name string) string {
	return `file("${projectDir}/_assets/bindspecs/` + name + `.json")`
}

// genPipeProcesses emits the BIND/FORK/MERGE/DISABLE helper processes for a
// pipeline's calls and its return binding.
func genPipeProcesses(b *strings.Builder, p *ir.Pipeline, prog *ir.Program, g genCtx) {
	for _, c := range p.Calls {
		if c.Mapped {
			genForkBindProcess(b, p.Name, c, g)
			genMergeProcess(b, p.Name, c, calleeOutNames(prog, c.Callable), g)

			if c.Disabled != nil {
				genDisableProcess(b, p.Name, c, g)
			}

			continue
		}

		genBindProcess(b, bindName(p.Name, c.Name), c.Bindings, g, g.producerArgs(c.Callable, types.RoleIn))

		if c.Disabled != nil {
			genDisableProcess(b, p.Name, c, g)
		}
	}

	// The return bind builds the pipeline's own output bundle.
	genBindProcess(b, bindName(p.Name, "return"), p.Returns, g, g.producerArgs(p.Name, types.RoleOut))
}

func genStage(b *strings.Builder, s *ir.Stage, g genCtx) {
	code := g.code[s.Name]
	mainOuts := strings.Join(append(names(s.Out), names(s.ChunkOut)...), ",")
	joinOuts := strings.Join(names(s.Out), ",")
	base := g.stageCmd("main", code, s.Lang)

	if !s.Split {
		genSingleStage(b, s, base, joinOuts, g)
		genKeyedSingleStage(b, s, base, joinOuts, g)

		return
	}

	genSplitProcesses(b, s, g, base, mainOuts, joinOuts)
	genSplitWorkflow(b, s)
	// A fork-key-threaded variant, used when this split stage is a map-call
	// target so each fork runs its own split/main/join and gathers per fork.
	genKeyedSplitProcesses(b, s, g, base, mainOuts, joinOuts)
	genKeyedSplitWorkflow(b, s)
}

// genKeyedSplitProcesses emits fork-key-carrying variants of the split/main/join
// processes: every channel item is tuple(key, ...), so chunks and joins stay
// partitioned by fork. Outputs are named by key so the merge orders them.
func genKeyedSplitProcesses(b *strings.Builder, s *ir.Stage, g genCtx, base, mainOuts, joinOuts string) {
	splitCmd := g.stageCmd("split", g.code[s.Name], s.Lang)
	joinCmd := g.stageCmd("join", g.code[s.Name], s.Lang)

	fmt.Fprintf(b, `process %[1]s_SPLIT_K {
%[2]s
  input:
    tuple val(key), path(args)
    path 'types.json'
  output:
    tuple val(key), path('chunks.json'), emit: defs
    tuple val(key), path('joinres.json'), emit: joinres
    tuple val(key), path('chunk_*', type: 'dir'), emit: chunks, optional: true
  script:
    """
    %[4]s -args ${args} -work . -o chunks.json -joinres joinres.json -chunkdir . -threads ${task.cpus} -memgb ${task.memory.toGiga()}%[5]s
    """
}

process %[1]s_MAIN_K {
%[12]s
  input:
    tuple val(key), val(res), path(chunk), path(args)
    path 'types.json'
  output:
    tuple val(key), path("out_${chunk.baseName}", type: 'dir')
  script:
    """
    %[6]s -args ${args} -chunk ${chunk} -outs '%[7]s'%[9]s -threads ${task.cpus} -memgb ${task.memory.toGiga()} -work . -o out_${chunk.baseName}
    """
}

process %[1]s_JOIN_K {
%[13]s
  input:
    tuple val(key), val(join), path(souts), path(args), path(defs)
    path 'types.json'
  output:
    tuple val(key), path("outs__${key}", type: 'dir')
  script:
    """
    %[8]s -args ${args} -chunkdefs ${defs} -chunkouts "\$(ls -1d out_* 2>/dev/null | sort -V | paste -sd, -)" -outs '%[10]s'%[11]s -threads ${task.cpus} -memgb ${task.memory.toGiga()} -work . -o outs__${key}
    """
}

`, s.Name, stageDirectives(s, ""), memOf(s), splitCmd, g.producerArgs(s.Name, types.RoleChunkIn),
		base, mainOuts, joinCmd, g.producerArgs(s.Name, types.RoleMainOut),
		joinOuts, g.producerArgs(s.Name, types.RoleOut),
		stageDirectives(s, "res"), stageDirectives(s, "join"))
}

// genKeyedSplitWorkflow wires the keyed split processes. multiMap duplicates the
// keyed args so split, main, and join each consume it without exhausting the
// queue; combine/join by the fork key keep chunks and outputs grouped per fork.
func genKeyedSplitWorkflow(b *strings.Builder, s *ir.Stage) {
	fmt.Fprintf(b, `workflow wf_%[1]s_map {
  take: keyed
  main:
    types = file("${projectDir}/_assets/types.json")
    ch = keyed.multiMap { k, a -> sp: tuple(k, a); mn: tuple(k, a); jn: tuple(k, a) }
    %[1]s_SPLIT_K(ch.sp, types)
    chunks = %[1]s_SPLIT_K.out.chunks.flatMap { key, cs -> (cs instanceof List ? cs : [cs]).collect { c -> tuple(key, new groovy.json.JsonSlurper().parseText(c.resolve('data.json').text).resources, c) } }
    %[1]s_MAIN_K(chunks.combine(ch.mn, by: 0), types)
    joinres = %[1]s_SPLIT_K.out.joinres.map { key, f -> tuple(key, new groovy.json.JsonSlurper().parseText(f.text)) }
    // Drive the join from the full fork set (ch.jn) with a remainder join on the
    // chunk outputs, so a fork whose split produced zero chunks (no groupTuple
    // group) still runs JOIN_K — with an empty chunk-outs list — instead of
    // being dropped. defs/joinres are inner-joined (always emitted per fork).
    joined = ch.jn.join(%[1]s_SPLIT_K.out.defs).join(joinres).join(%[1]s_MAIN_K.out.groupTuple(), remainder: true).map { t -> tuple(t[0], t[3], t[4] ?: [], t[1], t[2]) }
    %[1]s_JOIN_K(joined, types)
  emit:
    %[1]s_JOIN_K.out
}

`, s.Name)
}

func genSingleStage(b *strings.Builder, s *ir.Stage, base, outs string, g genCtx) {
	fmt.Fprintf(b, `process %[1]s {
%[2]s
  input:
    path args
    path 'types.json'
  output:
    path "outs__${args.baseName}", type: 'dir'
  script:
    """
    %[3]s -args ${args} -outs '%[4]s'%[5]s -threads ${task.cpus} -memgb ${task.memory.toGiga()} -work . -o outs__${args.baseName}
    """
}

workflow wf_%[1]s {
  take: args
  main:
    types = file("${projectDir}/_assets/types.json")
    %[1]s(args, types)
  emit:
    %[1]s.out
}

`, s.Name, stageDirectives(s, ""), base, outs, g.producerArgs(s.Name, types.RoleMainOut))
}

// genKeyedSingleStage emits a fork-key-threaded variant of a non-split stage:
// the process carries tuple(key, args) and names its output bundle by key so a
// map call (or an enclosing keyed pipeline) can run one instance per fork and
// gather per fork.
func genKeyedSingleStage(b *strings.Builder, s *ir.Stage, base, outs string, g genCtx) {
	fmt.Fprintf(b, `process %[1]s_MAP {
%[2]s
  input:
    tuple val(key), path(args)
    path 'types.json'
  output:
    tuple val(key), path("outs__${key}", type: 'dir')
  script:
    """
    %[3]s -args ${args} -outs '%[4]s'%[5]s -threads ${task.cpus} -memgb ${task.memory.toGiga()} -work . -o outs__${key}
    """
}

workflow wf_%[1]s_map {
  take: keyed
  main:
    types = file("${projectDir}/_assets/types.json")
    %[1]s_MAP(keyed, types)
  emit:
    %[1]s_MAP.out
}

`, s.Name, stageDirectives(s, ""), base, outs, g.producerArgs(s.Name, types.RoleMainOut))
}

func genSplitProcesses(b *strings.Builder, s *ir.Stage, g genCtx, base, mainOuts, joinOuts string) {
	splitCmd := g.stageCmd("split", g.code[s.Name], s.Lang)
	joinCmd := g.stageCmd("join", g.code[s.Name], s.Lang)

	fmt.Fprintf(b, `process %[1]s_SPLIT {
%[2]s
  input:
    path args
    path 'types.json'
  output:
    path 'chunks.json', emit: defs
    path 'joinres.json', emit: joinres
    path 'chunk_*', emit: chunks, type: 'dir', optional: true
  script:
    """
    %[4]s -args ${args} -work . -o chunks.json -joinres joinres.json -chunkdir . -threads ${task.cpus} -memgb ${task.memory.toGiga()}%[9]s
    """
}

process %[1]s_MAIN {
%[12]s
  input:
    tuple val(res), path(chunk), path(args)
    path 'types.json'
  output:
    path "out_${chunk.baseName}", type: 'dir'
  script:
    """
    %[5]s -args ${args} -chunk ${chunk} -outs '%[6]s'%[10]s -threads ${task.cpus} -memgb ${task.memory.toGiga()} -work . -o out_${chunk.baseName}
    """
}

process %[1]s_JOIN {
%[13]s
  input:
    val join
    path args
    path defs
    path souts
    path 'types.json'
  output:
    path 'outs', type: 'dir'
  script:
    """
    %[7]s -args ${args} -chunkdefs ${defs} -chunkouts "\$(ls -1d out_* 2>/dev/null | sort -V | paste -sd, -)" -outs '%[8]s'%[11]s -threads ${task.cpus} -memgb ${task.memory.toGiga()} -work . -o outs
    """
}

`, s.Name, stageDirectives(s, ""), memOf(s), splitCmd, base, mainOuts, joinCmd, joinOuts,
		g.producerArgs(s.Name, types.RoleChunkIn),
		g.producerArgs(s.Name, types.RoleMainOut),
		g.producerArgs(s.Name, types.RoleOut),
		stageDirectives(s, "res"), stageDirectives(s, "join"))
}

func genSplitWorkflow(b *strings.Builder, s *ir.Stage) {
	fmt.Fprintf(b, `workflow wf_%[1]s {
  take: args
  main:
    types = file("${projectDir}/_assets/types.json")
    a = args
    %[1]s_SPLIT(a, types)
    chunks = %[1]s_SPLIT.out.chunks.flatten().map { f -> tuple(new groovy.json.JsonSlurper().parseText(f.resolve('data.json').text).resources, f) }
    %[1]s_MAIN(chunks.combine(a), types)
    join = %[1]s_SPLIT.out.joinres.map { f -> new groovy.json.JsonSlurper().parseText(f.text) }
    %[1]s_JOIN(join, a, %[1]s_SPLIT.out.defs, %[1]s_MAIN.out.collect(), types)
  emit:
    %[1]s_JOIN.out
}

`, s.Name)
}

// genDisableProcess emits a process that resolves a call's `disabled` flag into
// a disable bundle at runtime. It carries no file leaves, so it needs no
// producer flags.
func genDisableProcess(b *strings.Builder, pipeline string, c ir.Call, g genCtx) {
	block, arg := bindInputs(refCalls(disableBindings(c)))

	fmt.Fprintf(b, `process %[1]s {
  input:
%[2]s  output:
    path 'disable', type: 'dir'
  script:
    """
    '%[3]s' bind -spec 'spec.json' -pipeargs ${pipeargs}%[4]s -o disable
    """
}

`, disableName(pipeline, c.Name), block, g.mre, arg)
}

// bindInputs renders the input block and the -inputs argument for a bind/fork
// process: pipeline args plus one staged bundle per referenced upstream call.
func bindInputs(refs []string) (string, string) {
	var inputs strings.Builder

	inputs.WriteString("    path pipeargs\n")

	pairs := make([]string, 0, len(refs))
	for _, id := range refs {
		fmt.Fprintf(&inputs, "    path 'in_%s'\n", id)
		pairs = append(pairs, fmt.Sprintf("%s=in_%s", id, id))
	}

	// The shared type manifest and this process's own bindspec are staged as two
	// individual files (not the whole _assets dir), so a task transfers only the
	// one bindspec it needs rather than every call's spec; see assetsDir.
	inputs.WriteString("    path 'types.json'\n")
	inputs.WriteString("    path 'spec.json'\n")

	arg := ""
	if len(pairs) > 0 {
		arg = " -inputs " + strings.Join(pairs, ",")
	}

	return inputs.String(), arg
}

// genBindProcess emits a process that resolves one call's (or the return's)
// input bindings into an args bundle via `mre bind`. prodArgs stages any file
// leaves of the produced bundle (empty for the disable resolver).
func genBindProcess(b *strings.Builder, name string, bindings []ir.Binding, g genCtx, prodArgs string) {
	block, arg := bindInputs(refCalls(bindings))

	fmt.Fprintf(b, `process %[1]s {
  input:
%[2]s  output:
    path 'args', type: 'dir'
  script:
    """
    '%[3]s' bind -spec 'spec.json' -pipeargs ${pipeargs}%[4]s -o args%[5]s
    """
}

`, name, block, g.mre, arg, prodArgs)
}

// genForkBindProcess emits a process that resolves a map call's bindings into
// one args bundle per fork (fork_NNNNN/) via `mre forkbind`.
func genForkBindProcess(b *strings.Builder, pipeline string, c ir.Call, g genCtx) {
	block, arg := bindInputs(refCalls(c.Bindings))

	fmt.Fprintf(b, `process %[1]s {
  input:
%[2]s  output:
    path 'fork_*', emit: forks, type: 'dir', optional: true
    path 'forkkeys.json', emit: keys
  script:
    """
    '%[3]s' forkbind -spec 'spec.json' -pipeargs ${pipeargs}%[4]s -chunkdir .%[5]s
    """
}

`, forkName(pipeline, c.Name), block, g.mre, arg, g.producerArgs(c.Callable, types.RoleIn))
}

// genMergeProcess emits a process that merges per-fork outputs into the
// map-call result bundle via `mre merge`.
func genMergeProcess(b *strings.Builder, pipeline string, c ir.Call, calleeOuts string, g genCtx) {
	fmt.Fprintf(b, `process %[1]s {
  input:
    path souts
    path 'forkkeys.json'
    path 'types.json'
  output:
    path 'merged', type: 'dir'
  script:
    """
    '%[2]s' merge -outs '%[3]s' -files "\$(ls -1d outs__* 2>/dev/null | sort -V | paste -sd, -)" -keys-file forkkeys.json -o merged%[4]s
    """
}

`, mergeName(pipeline, c.Name), g.mre, calleeOuts, g.producerArgs(c.Callable, types.RoleOut))
}

func genPipelineWorkflow(b *strings.Builder, p *ir.Pipeline) {
	var body strings.Builder

	// pipeargs is always a value channel (the entry uses Channel.value and nested
	// calls pass the parent's value-channel pa), so it is directly re-readable by
	// every bind — no .first() needed (which would warn as useless on a value).
	body.WriteString("  main:\n    pa = pipeargs\n")
	body.WriteString("    types = file(\"${projectDir}/_assets/types.json\")\n")

	for _, c := range p.Calls {
		genCallWiring(&body, p.Name, c)
	}

	retName := bindName(p.Name, "return")
	fmt.Fprintf(&body, "    %s(%s)\n", retName, bindCallArgs(p.Returns, retName))

	fmt.Fprintf(b, `workflow %s {
  take: pipeargs
%s  emit:
    %s.out
}

`, p.Name, body.String(), retName)
}

// genCallWiring emits the wiring for one call: a map-call fork/merge fan-out, a
// disabled-aware branch, or a plain BIND + callee invocation.
func genCallWiring(b *strings.Builder, pipeline string, c ir.Call) {
	callee := callAlias(pipeline, c.Name)

	if c.Mapped {
		genMappedWiring(b, pipeline, c, callee)

		return
	}

	bind := bindName(pipeline, c.Name)
	fmt.Fprintf(b, "    %s(%s)\n", bind, bindCallArgs(c.Bindings, bind))

	if c.Disabled != nil {
		genDisabledWiring(b, pipeline, c, callee)

		return
	}

	// Bind outputs are value channels (every input traces back to the
	// Channel.value entry args through all-value-input processes), so the callee
	// result is itself a value channel — reusable by multiple downstream
	// consumers without .first().
	fmt.Fprintf(b, "    ch_%s = %s(%s.out)\n", c.Name, callee, bind)
}

// genMappedWiring emits a map call's fork/callee/merge fan-out. A split-stage
// callee runs through its fork-key-threaded variant (each fork keyed by its
// args-bundle name); other callees take the flattened fork channel directly.
func genMappedWiring(b *strings.Builder, pipeline string, c ir.Call, callee string) {
	fork := forkName(pipeline, c.Name)
	merge := mergeName(pipeline, c.Name)

	// When the map call is disabled, gate the fork's pipeline-args by the runtime
	// flag so the forks only run when enabled; on skip the call yields its null
	// outputs bundle instead.
	// The FORK process reuses the call's bind spec (see genForkBindProcess).
	forkArgs := bindCallArgs(c.Bindings, bindName(pipeline, c.Name))
	if c.Disabled != nil {
		forkArgs = genMappedDisableGate(b, pipeline, c)
	}

	fmt.Fprintf(b, "    %s(%s)\n", fork, forkArgs)
	// Each fork is keyed by its args-bundle name and run through the callee's
	// fork-key-threaded variant, which emits tuple(key, outBundle).
	fmt.Fprintf(b, "    keyed_%s = %s.out.forks.flatten().map { f -> tuple(f.baseName, f) }\n", c.Name, fork)
	fmt.Fprintf(b, "    out_%s = %s(keyed_%s).map { k, bundle -> bundle }\n", c.Name, callee, c.Name)
	// ifEmpty([]) ensures MERGE still runs for an empty fork collection
	// (collect() on an empty channel emits nothing), yielding null outputs.
	// FORK.out.keys carries map-fork keys (null for an array fork).
	fmt.Fprintf(b, "    %s(out_%s.collect().ifEmpty([]), %s.out.keys, types)\n", merge, c.Name, fork)

	if c.Disabled != nil {
		// On the disabled branch FORK is skipped, so MERGE produces nothing; mix
		// in the null bundle. .first() makes the merged result reusable.
		nulls := "${projectDir}/nulls/" + qualify(pipeline, c.Name)
		fmt.Fprintf(b, "    s_%[1]s = g_%[1]s.skip.map { a, d -> file(\"%[2]s\") }\n", c.Name, nulls)
		fmt.Fprintf(b, "    ch_%s = %s.out.mix(s_%s).first()\n", c.Name, merge, c.Name)

		return
	}

	// MERGE's inputs are value channels (collected forks + keys), so its output
	// is a value channel reusable by multiple downstream consumers.
	fmt.Fprintf(b, "    ch_%s = %s.out\n", c.Name, merge)
}

// genMappedDisableGate emits the DISABLE process and a run/skip branch on the
// resolved flag, returning the fork's actual-args with pipeargs replaced by the
// enabled (run) branch.
func genMappedDisableGate(b *strings.Builder, pipeline string, c ir.Call) string {
	dis := disableName(pipeline, c.Name)
	fmt.Fprintf(b, "    %s(%s)\n", dis, bindCallArgs(disableBindings(c), dis))
	fmt.Fprintf(b, `    g_%[1]s = pa.combine(%[2]s.out).branch { a, d ->
        def off = new groovy.json.JsonSlurper().parseText(d.resolve('data.json').text).disabled
        run: !off
        skip: off
    }
`, c.Name, dis)

	// Feeds the FORK process, which reuses the call's bind spec.
	return bindCallArgsPa(c.Bindings, fmt.Sprintf("g_%s.run.map { a, d -> a }", c.Name), bindName(pipeline, c.Name))
}

// genDisabledWiring runs the callee only when the resolved `disabled` flag is
// false; disabled forks emit a null outputs bundle instead.
func genDisabledWiring(b *strings.Builder, pipeline string, c ir.Call, callee string) {
	bind := bindName(pipeline, c.Name)
	dis := disableName(pipeline, c.Name)
	nulls := "${projectDir}/nulls/" + qualify(pipeline, c.Name)

	fmt.Fprintf(b, "    %s(%s)\n", dis, bindCallArgs(disableBindings(c), dis))
	fmt.Fprintf(b, `    g_%[1]s = %[2]s.out.combine(%[3]s.out).branch { a, d ->
        def off = new groovy.json.JsonSlurper().parseText(d.resolve('data.json').text).disabled
        run: !off
        skip: off
    }
    r_%[1]s = %[4]s(g_%[1]s.run.map { a, d -> a })
    s_%[1]s = g_%[1]s.skip.map { a, d -> file("%[5]s") }
    ch_%[1]s = r_%[1]s.mix(s_%[1]s).first()
`, c.Name, bind, dis, callee, nulls)
}

// calleeModule returns the exported workflow name and module path for a
// callable: stages export wf_<stage> from stage_<stage>.nf; pipelines export
// <pipeline> from pipe_<pipeline>.nf.
func calleeModule(prog *ir.Program, callable string) (string, string) {
	if _, ok := prog.Stages[callable]; ok {
		return "wf_" + callable, "./stage_" + callable + ".nf"
	}

	return callable, "./pipe_" + callable + ".nf"
}

// callAlias is the per-call workflow alias, unique within a pipeline, so each
// call (including repeated/aliased calls) is an independent instance.
func callAlias(pipeline, call string) string {
	return "wf_" + qualify(pipeline, call)
}

// disableBindings builds a one-entry binding list for a call's disabled ref.
func disableBindings(c ir.Call) []ir.Binding {
	return []ir.Binding{{Param: "disabled", Value: ir.Value{Ref: c.Disabled}}}
}

func disableName(pipeline, call string) string {
	return "DISABLE_" + qualify(pipeline, call)
}

// bindCallArgs renders the actual-argument list for a BIND invocation: the
// pipeline args first, then each referenced upstream call's output channel.
// specName is the bind process's own bindspec (staged as spec.json).
func bindCallArgs(bindings []ir.Binding, specName string) string {
	return bindCallArgsPa(bindings, "pa", specName)
}

// bindCallArgsPa is bindCallArgs with an explicit pipeline-args channel
// expression (used to substitute a gated channel for a disabled call).
func bindCallArgsPa(bindings []ir.Binding, pa, specName string) string {
	refs := refCalls(bindings)

	args := make([]string, 0, len(refs)+1)
	args = append(args, pa)

	for _, id := range refs {
		args = append(args, "ch_"+id)
	}

	// Every bind/fork/disable process takes two final broadcast inputs: the shared
	// type manifest (types) and its own bindspec (spec.json); see assetsDir. The
	// workflow defines `types` in its main block.
	args = append(args, "types", specFile(specName))

	return strings.Join(args, ", ")
}

// genPublish emits the PUBLISH process: it reads the entry's final output
// bundle, copies every file-typed output (including nested ones) into the
// results dir under its basename, and writes the published outs JSON. The staged
// bundle is removed before output globbing so only published artifacts remain.
func genPublish(b *strings.Builder, entry string, g genCtx) {
	fmt.Fprintf(b, `process PUBLISH {
  publishDir params.outdir ?: '.', mode: 'copy', enabled: params.outdir != null
  input:
    path bundle
    path 'types.json'
  output:
    path '*'
  script:
    """
    "%[1]s" publish -bundle "${bundle}" -dir .%[2]s
    rm -rf "${bundle}"
    """
}

`, g.mre, g.producerArgs(entry, types.RoleOut))
}

// genEntry emits the top-level workflow. Each entry input is exposed as a
// nullable run parameter (params.<name>); at launch the BUILD_ENTRY_ARGS process
// overlays the supplied values on the baked defaults, so inputs can be set via a
// Nextflow -params-file or AWS HealthOmics run parameters without re-transpiling.
//
// A file-bearing input (a file/dir at any dimension, or a struct/array/map whose
// leaves are files) is additionally routed through Nextflow's own staging: its
// file leaves (s3:// URIs or paths) are flattened to a list, file()'d on the head
// node, and declared as a `path` input, so the worker reads real local files (an
// isolated AWS Batch / HealthOmics task cannot stat the raw values). entryargs
// pops the staged paths back into the value in the canonical type-walk order and
// marks them into the bundle. An unset input is fed the empty sentinel and keeps
// its baked default.
func genEntry(b *strings.Builder, prog *ir.Program, entryWorkflow string, g genCtx) {
	ins := entryInParams(prog)

	var decls, fileInputs, fileChans strings.Builder

	// `[:]` is Groovy's empty map (a bare `[]` would be a list, which the overrides
	// JSON object must not be); a non-empty map lists each input's param.
	pairs := make([]string, 0, len(ins))
	flatFlags := make([]string, 0) // name=joined-staged-paths for the entryargs -fileflat flag
	callArgs := []string{`file("${projectDir}/entry_args")`, "values", "types"}
	sentinel := fmt.Sprintf(`file("${projectDir}/%s/%s")`, assetsDir, entrySentinel)

	for _, p := range ins {
		fmt.Fprintf(&decls, "params.%s = null\n", p.Name)
		pairs = append(pairs, fmt.Sprintf("%[1]s: params.%[1]s", p.Name))

		if !hasFileLeaf(p, prog.Structs) {
			continue
		}

		in := "inflat_" + p.Name
		// stageAs '<in>_?/*' stages each file leaf into its own numbered subdir
		// (<in>_1/<basename>, <in>_2/<basename>, ...), so leaves that share a
		// basename (e.g. sampleA/reads.fastq + sampleB/reads.fastq) do not collide
		// in the task work dir, while the original basename is preserved for the
		// stage. The order matches the head-node flatten (canonical type order).
		fmt.Fprintf(&fileInputs, "    path(%s, stageAs: '%s_?/*')\n", in, in)
		// The staged paths (one per file leaf, canonical order) reach entryargs joined
		// by ','; multiple inputs are ';'-separated and the whole flag is single-quoted
		// so the shell leaves it intact while Nextflow still interpolates the ${...}.
		// The list-coercion matters: a single staged file is a lone Groovy Path, and
		// Path.join(",") iterates its path *segments* (the stageAs subdir splits it
		// into "<in>_1,<basename>"); wrapping it in a list makes join treat the whole
		// path as one element. A multi-leaf input is already a List, left as-is.
		flatFlags = append(flatFlags, fmt.Sprintf("%[1]s=${(%[2]s instanceof List ? %[2]s : [%[2]s]).join(\",\")}", p.Name, in))
		callArgs = append(callArgs, "flat_"+p.Name)
		// Flatten the override's file leaves to a list of staged files on the head node;
		// an unset input (or one with no file leaves) falls back to the empty sentinel so
		// the process still runs (entryargs ignores the sentinel when the input is unset).
		fmt.Fprintf(&fileChans, "  flat_%[1]s = (params.%[1]s != null ? (%[2]s) : []) ?: [%[3]s]\n",
			p.Name, fileFlattenExpr("params."+p.Name, p, prog.Structs), sentinel)
	}

	valuesMap := "[:]"
	if len(pairs) > 0 {
		valuesMap = "[" + strings.Join(pairs, ", ") + "]"
	}

	flatFlag := ""
	if len(flatFlags) > 0 {
		flatFlag = " -fileflat '" + strings.Join(flatFlags, ";") + "'"
	}

	// A quoted heredoc writes the overrides to a file; Nextflow interpolates the
	// JSON string, and the quoted delimiter stops the shell from expanding it.
	fmt.Fprintf(b, `%[1]s
process BUILD_ENTRY_ARGS {
  input:
    path 'entry_args'
    val values
    path 'types.json'
%[6]s  output:
    path 'entry_resolved', type: 'dir'
  script:
    """
    cat > values.json <<'MART_EOF'
${values}
MART_EOF
    '%[2]s' entryargs -base entry_args -values values.json -o entry_resolved -types 'types.json' -callable '%[3]s' -role in%[7]s
    """
}

workflow {
  types = file("${projectDir}/_assets/types.json")
  values = groovy.json.JsonOutput.toJson(%[4]s)
%[8]s  BUILD_ENTRY_ARGS(%[9]s)
  pipeargs = BUILD_ENTRY_ARGS.out.first()
  %[5]s(pipeargs)
  PUBLISH(%[5]s.out, types)
}
`, decls.String(), g.mre, prog.Entry.Callable, valuesMap, entryWorkflow,
		fileInputs.String(), flatFlag, fileChans.String(), strings.Join(callArgs, ", "))
}

// fileFlattenExpr renders a Groovy expression that flattens the file leaves of a
// runtime value (expr, of param p's type) into a list of file() objects, in the
// canonical walk order types.Table uses — arrays in index order, maps by sorted
// key, struct fields in declaration order — so mre entryargs can pop the staged
// paths back into the value in the same order. Non-file scalars contribute [].
func fileFlattenExpr(expr string, p ir.Param, structs map[string]*ir.StructType) string {
	switch {
	case p.ArrayDim > 0:
		elem := p
		elem.ArrayDim--

		return fmt.Sprintf("(%s ?: []).collect { __e -> %s }.flatten()", expr, fileFlattenExpr("__e", elem, structs))
	case p.MapDim > 0:
		val := p
		val.MapDim--

		return fmt.Sprintf("(%s ?: [:]).sort { it.key }.collect { __e -> %s }.flatten()", expr, fileFlattenExpr("__e.value", val, structs))
	}

	if st, ok := structs[p.BaseType]; ok {
		parts := make([]string, 0, len(st.Fields))
		for _, f := range st.Fields {
			parts = append(parts, fileFlattenExpr(fmt.Sprintf("(%s)?.%s", expr, f.Name), f, structs))
		}

		if len(parts) == 0 {
			return "[]"
		}

		return "(" + strings.Join(parts, " + ") + ")"
	}

	if p.IsFile {
		return fmt.Sprintf("(%[1]s != null ? [file(%[1]s)] : [])", expr)
	}

	return "[]"
}

// qualify builds a collision-free, valid-identifier suffix from a (pipeline,
// call) pair. The length prefix disambiguates pairs whose names themselves
// contain the "__" separator (e.g. pipeline "A"/call "B__C" vs "A__B"/"C").
func qualify(pipeline, call string) string {
	return strconv.Itoa(len(pipeline)) + "_" + pipeline + "__" + call
}

func bindName(pipeline, call string) string {
	return "BIND_" + qualify(pipeline, call)
}

func forkName(pipeline, call string) string {
	return "FORK_" + qualify(pipeline, call)
}

func mergeName(pipeline, call string) string {
	return "MERGE_" + qualify(pipeline, call)
}

// calleeOutNames returns the comma-joined output parameter names of a callable.
func calleeOutNames(prog *ir.Program, callable string) string {
	if s, ok := prog.Stages[callable]; ok {
		return strings.Join(names(s.Out), ",")
	}

	if p, ok := prog.Pipelines[callable]; ok {
		return strings.Join(names(p.Out), ",")
	}

	return ""
}

// refCalls returns the unique, sorted upstream call ids referenced anywhere in
// the bindings' value trees (including refs nested inside array/object literals).
func refCalls(bindings []ir.Binding) []string {
	seen := map[string]bool{}

	var ids []string

	var walk func(v ir.Value)
	walk = func(v ir.Value) {
		if v.Ref != nil && v.Ref.Kind == "call" && !seen[v.Ref.ID] {
			seen[v.Ref.ID] = true
			ids = append(ids, v.Ref.ID)
		}

		for _, e := range v.Array {
			walk(e)
		}

		for _, e := range v.Object {
			walk(e)
		}
	}

	for _, bnd := range bindings {
		walk(bnd.Value)
	}

	sort.Strings(ids)

	return ids
}

func names(params []ir.Param) []string {
	out := make([]string, 0, len(params))
	for _, p := range params {
		out = append(out, p.Name)
	}

	return out
}

// stageDirectives renders the scheduler directives (cpus, memory, and an
// optional clusterOptions for `special`) at the top of a stage-phase process.
// When val is empty the cpus/memory are static (split/single-stage phases);
// otherwise they read the per-task val (a chunk's resolved resources for main,
// the split-returned override for join). Each line carries the process body's
// two-space indent.
func stageDirectives(s *ir.Stage, val string) string {
	var b strings.Builder

	if val == "" {
		fmt.Fprintf(&b, "  cpus %d\n  %s", cpusOf(s), staticMem(memOf(s)))
	} else {
		fmt.Fprintf(&b, "  %s\n  %s", dynCpus(val, cpusOf(s)), dynMem(val, memOf(s)))
	}

	// `special` is mrp's scheduler-routing key (MRO_JOBRESOURCES); map it through
	// params.job_resources so a grid run resolves it to clusterOptions. Emitted
	// only when the stage declares one statically; a per-task __special override
	// (split-returned) then wins over the static key at runtime.
	if n, ok := gpuRequest(s.Resources.Special); ok {
		// Reserved `special = "gpu[:N]"`: request N whole GPUs via the accelerator
		// directive, on the compute phase only — a split stage's split and join
		// phases do no GPU work. The awsbatch/healthomics executors translate
		// accelerator into a GPU resourceRequirement; see docs/GPU.md for the
		// backend (compute environment / queue) setup.
		if (!s.Split && val == "") || (s.Split && val == "res") {
			fmt.Fprintf(&b, "\n  accelerator %d", n)
		}
	} else if s.Resources.Special != "" {
		key := "'" + s.Resources.Special + "'"
		if val != "" {
			key = fmt.Sprintf("%s?.special ?: '%s'", val, s.Resources.Special)
		}

		fmt.Fprintf(&b, "\n  clusterOptions { params.job_resources?.get(%s) ?: '' }", key)
	}

	return b.String()
}

// gpuRequest parses the reserved `special` value for a GPU request: "gpu" -> 1,
// "gpu:N" -> N (a positive integer). Any other value returns ok=false, keeping
// the clusterOptions scheduler-key routing. A GPU is a whole device, so the
// request is a count with no size dimension; the GPU type is a property of the
// compute environment, not the .mro (see docs/GPU.md).
func gpuRequest(special string) (int, bool) {
	if special == "gpu" {
		return 1, true
	}

	if rest, ok := strings.CutPrefix(special, "gpu:"); ok {
		if n, err := strconv.Atoi(rest); err == nil && n > 0 {
			return n, true
		}
	}

	return 0, false
}

// dynCpus renders a dynamic `cpus` directive: it provisions the runtime val's
// threads (rounded up to a whole CPU) when positive, else the stage's static
// `using` default. val is the name of a per-task val input (e.g. a chunk's
// resolved resources or the split-returned join override).
func dynCpus(val string, fallback int) string {
	return fmt.Sprintf("cpus { (%[1]s?.threads ?: 0) > 0 ? Math.max(1, Math.ceil(%[1]s.threads as double) as int) : %[2]d }", val, fallback)
}

// dynMem renders a dynamic `memory` directive: the runtime val's mem_gb when
// positive, else the stage's static `using` default.
func dynMem(val string, fallback int) string {
	return fmt.Sprintf("memory { def m = (%[1]s?.mem_gb ?: 0) > 0 ? %[1]s.mem_gb : %[2]d; (m * task.attempt) + ' GB' }", val, fallback)
}

// staticMem renders a static `memory` directive that grows with task.attempt —
// the --auto-adjust-memory analog. Attempt 1 requests the stage's `using` value;
// a retry (an OOM kill is a retryable, non-ASSERT failure) requests a multiple,
// so a stage that died for want of memory gets more on the next attempt instead
// of failing identically. cpus do not escalate (more CPUs do not fix an OOM).
func staticMem(memGB int) string {
	return fmt.Sprintf("memory { %d * task.attempt + ' GB' }", memGB)
}

func cpusOf(s *ir.Stage) int {
	if s.Resources.Threads < 1 {
		return 1
	}

	return int(math.Ceil(s.Resources.Threads))
}

func memOf(s *ir.Stage) int {
	if s.Resources.MemGB < 1 {
		return 1
	}

	return int(math.Ceil(s.Resources.MemGB))
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	return keys
}
