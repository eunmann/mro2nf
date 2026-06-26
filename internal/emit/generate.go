package emit

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/eunmann/martian-nextflow/internal/ir"
	"github.com/eunmann/martian-nextflow/internal/types"
)

// genCtx carries the resolved paths and names needed to render Nextflow code.
type genCtx struct {
	entry   string
	mroFile string
	mre     string
	shell   string
	mrjob   string
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

	return cmd
}

// producerArgs renders the flags a bundle-producing mre command needs to stage
// the file leaves of callable's params under the given role.
func (g genCtx) producerArgs(callable, role string) string {
	return fmt.Sprintf(" -types '${projectDir}/types.json' -callable '%s' -role %s", callable, role)
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
	genPipeProcesses(&b, p, prog, g)
	genPipelineWorkflow(&b, p, prog)

	return b.String()
}

// generateMain renders main.nf: the entry workflow plus PUBLISH. The entry
// callable may be a pipeline or a bare stage.
func generateMain(prog *ir.Program, g genCtx) string {
	var b strings.Builder

	export, src := calleeModule(prog, prog.Entry.Callable)

	fmt.Fprintf(&b, "include { %s } from './modules/%s'\n\n", export, strings.TrimPrefix(src, "./"))
	genPublish(&b, prog.Entry.Callable, g)
	genEntry(&b, export)

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
	if c.Mapped && isSplitStage(prog, c.Callable) {
		export = "wf_" + c.Callable + "_map"
	}

	return export, src
}

// isSplitStage reports whether callable names a split stage.
func isSplitStage(prog *ir.Program, callable string) bool {
	s, ok := prog.Stages[callable]

	return ok && s.Split
}

// genPipeProcesses emits the BIND/FORK/MERGE/DISABLE helper processes for a
// pipeline's calls and its return binding.
func genPipeProcesses(b *strings.Builder, p *ir.Pipeline, prog *ir.Program, g genCtx) {
	for _, c := range p.Calls {
		if c.Mapped {
			genForkBindProcess(b, p.Name, c, g)
			genMergeProcess(b, p.Name, c, calleeOutNames(prog, c.Callable), g)

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
  cpus %[2]d
  memory '%[3]d GB'
  input:
    tuple val(key), path(args)
  output:
    tuple val(key), path('chunks.json'), emit: defs
    tuple val(key), path('chunk_*', type: 'dir'), emit: chunks, optional: true
  script:
    """
    %[4]s -args ${args} -work . -o chunks.json -chunkdir .%[5]s
    """
}

process %[1]s_MAIN_K {
  cpus { (res?.threads ?: 0) > 0 ? Math.max(1, Math.ceil(res.threads as double) as int) : %[2]d }
  memory { (res?.mem_gb ?: 0) > 0 ? "${res.mem_gb} GB" : '%[3]d GB' }
  input:
    tuple val(key), val(res), path(chunk), path(args)
  output:
    tuple val(key), path("out_${chunk.baseName}", type: 'dir')
  script:
    """
    %[6]s -args ${args} -chunk ${chunk} -outs '%[7]s'%[9]s -threads ${task.cpus} -memgb ${task.memory.toGiga()} -work . -o out_${chunk.baseName}
    """
}

process %[1]s_JOIN_K {
  cpus %[2]d
  memory '%[3]d GB'
  input:
    tuple val(key), path(souts), path(args), path(defs)
  output:
    tuple val(key), path("outs__${key}", type: 'dir')
  script:
    """
    %[8]s -args ${args} -chunkdefs ${defs} -chunkouts "\$(ls -1d out_* 2>/dev/null | sort -V | paste -sd, -)" -outs '%[10]s'%[11]s -work . -o outs__${key}
    """
}

`, s.Name, cpusOf(s), memOf(s), splitCmd, g.producerArgs(s.Name, types.RoleChunkIn),
		base, mainOuts, joinCmd, g.producerArgs(s.Name, types.RoleMainOut),
		joinOuts, g.producerArgs(s.Name, types.RoleOut))
}

// genKeyedSplitWorkflow wires the keyed split processes. multiMap duplicates the
// keyed args so split, main, and join each consume it without exhausting the
// queue; combine/join by the fork key keep chunks and outputs grouped per fork.
func genKeyedSplitWorkflow(b *strings.Builder, s *ir.Stage) {
	fmt.Fprintf(b, `workflow wf_%[1]s_map {
  take: keyed
  main:
    ch = keyed.multiMap { k, a -> sp: tuple(k, a); mn: tuple(k, a); jn: tuple(k, a) }
    %[1]s_SPLIT_K(ch.sp)
    chunks = %[1]s_SPLIT_K.out.chunks.flatMap { key, cs -> (cs instanceof List ? cs : [cs]).collect { c -> tuple(key, new groovy.json.JsonSlurper().parseText(file("${c}/data.json").text).resources, c) } }
    %[1]s_MAIN_K(chunks.combine(ch.mn, by: 0))
    joined = %[1]s_MAIN_K.out.groupTuple().join(ch.jn).join(%[1]s_SPLIT_K.out.defs)
    %[1]s_JOIN_K(joined)
  emit:
    %[1]s_JOIN_K.out
}

`, s.Name)
}

func genSingleStage(b *strings.Builder, s *ir.Stage, base, outs string, g genCtx) {
	fmt.Fprintf(b, `process %[1]s {
  cpus %[2]d
  memory '%[3]d GB'
  input:
    path args
  output:
    path "outs__${args.baseName}"
  script:
    """
    %[4]s -args ${args} -outs '%[5]s'%[6]s -threads ${task.cpus} -memgb ${task.memory.toGiga()} -work . -o outs__${args.baseName}
    """
}

workflow wf_%[1]s {
  take: args
  main:
    %[1]s(args)
  emit:
    %[1]s.out
}

`, s.Name, cpusOf(s), memOf(s), base, outs, g.producerArgs(s.Name, types.RoleMainOut))
}

func genSplitProcesses(b *strings.Builder, s *ir.Stage, g genCtx, base, mainOuts, joinOuts string) {
	splitCmd := g.stageCmd("split", g.code[s.Name], s.Lang)
	joinCmd := g.stageCmd("join", g.code[s.Name], s.Lang)

	fmt.Fprintf(b, `process %[1]s_SPLIT {
  cpus %[2]d
  memory '%[3]d GB'
  input:
    path args
  output:
    path 'chunks.json', emit: defs
    path 'chunk_*', emit: chunks, type: 'dir', optional: true
  script:
    """
    %[4]s -args ${args} -work . -o chunks.json -chunkdir .%[9]s
    """
}

process %[1]s_MAIN {
  cpus { (res?.threads ?: 0) > 0 ? Math.max(1, Math.ceil(res.threads as double) as int) : %[2]d }
  memory { (res?.mem_gb ?: 0) > 0 ? "${res.mem_gb} GB" : '%[3]d GB' }
  input:
    tuple val(res), path(chunk), path(args)
  output:
    path "out_${chunk.baseName}", type: 'dir'
  script:
    """
    %[5]s -args ${args} -chunk ${chunk} -outs '%[6]s'%[10]s -threads ${task.cpus} -memgb ${task.memory.toGiga()} -work . -o out_${chunk.baseName}
    """
}

process %[1]s_JOIN {
  cpus %[2]d
  memory '%[3]d GB'
  input:
    path args
    path defs
    path souts
  output:
    path 'outs', type: 'dir'
  script:
    """
    %[7]s -args ${args} -chunkdefs ${defs} -chunkouts "\$(ls -1d out_* 2>/dev/null | sort -V | paste -sd, -)" -outs '%[8]s'%[11]s -work . -o outs
    """
}

`, s.Name, cpusOf(s), memOf(s), splitCmd, base, mainOuts, joinCmd, joinOuts,
		g.producerArgs(s.Name, types.RoleChunkIn),
		g.producerArgs(s.Name, types.RoleMainOut),
		g.producerArgs(s.Name, types.RoleOut))
}

func genSplitWorkflow(b *strings.Builder, s *ir.Stage) {
	fmt.Fprintf(b, `workflow wf_%[1]s {
  take: args
  main:
    a = args.first()
    %[1]s_SPLIT(a)
    chunks = %[1]s_SPLIT.out.chunks.flatten().map { f -> tuple(new groovy.json.JsonSlurper().parseText(file("${f}/data.json").text).resources, f) }
    %[1]s_MAIN(chunks.combine(a))
    %[1]s_JOIN(a, %[1]s_SPLIT.out.defs, %[1]s_MAIN.out.collect())
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
    '%[3]s' bind -spec '${projectDir}/bindspecs/%[1]s.json' -pipeargs ${pipeargs}%[4]s -o disable
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
    '%[3]s' bind -spec '${projectDir}/bindspecs/%[1]s.json' -pipeargs ${pipeargs}%[4]s -o args%[5]s
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
    '%[3]s' forkbind -spec '${projectDir}/bindspecs/%[4]s.json' -pipeargs ${pipeargs}%[5]s -chunkdir .%[6]s
    """
}

`, forkName(pipeline, c.Name), block, g.mre, bindName(pipeline, c.Name), arg, g.producerArgs(c.Callable, types.RoleIn))
}

// genMergeProcess emits a process that merges per-fork outputs into the
// map-call result bundle via `mre merge`.
func genMergeProcess(b *strings.Builder, pipeline string, c ir.Call, calleeOuts string, g genCtx) {
	fmt.Fprintf(b, `process %[1]s {
  input:
    path souts
    path 'forkkeys.json'
  output:
    path 'merged', type: 'dir'
  script:
    """
    '%[2]s' merge -outs '%[3]s' -files "\$(ls -1d outs__* 2>/dev/null | sort -V | paste -sd, -)" -keys-file forkkeys.json -o merged%[4]s
    """
}

`, mergeName(pipeline, c.Name), g.mre, calleeOuts, g.producerArgs(c.Callable, types.RoleOut))
}

func genPipelineWorkflow(b *strings.Builder, p *ir.Pipeline, prog *ir.Program) {
	var body strings.Builder

	body.WriteString("  main:\n    pa = pipeargs.first()\n")

	for _, c := range p.Calls {
		genCallWiring(&body, p.Name, c, prog)
	}

	retName := bindName(p.Name, "return")
	fmt.Fprintf(&body, "    %s(%s)\n", retName, bindCallArgs(p.Returns))

	fmt.Fprintf(b, `workflow %s {
  take: pipeargs
%s  emit:
    %s.out
}

`, p.Name, body.String(), retName)
}

// genCallWiring emits the wiring for one call: a map-call fork/merge fan-out, a
// disabled-aware branch, or a plain BIND + callee invocation.
func genCallWiring(b *strings.Builder, pipeline string, c ir.Call, prog *ir.Program) {
	callee := callAlias(pipeline, c.Name)

	if c.Mapped {
		genMappedWiring(b, pipeline, c, callee, prog)

		return
	}

	bind := bindName(pipeline, c.Name)
	fmt.Fprintf(b, "    %s(%s)\n", bind, bindCallArgs(c.Bindings))

	if c.Disabled != nil {
		genDisabledWiring(b, pipeline, c, callee)

		return
	}

	// .first() yields a value channel reusable by multiple downstream consumers.
	fmt.Fprintf(b, "    ch_%s = %s(%s.out).first()\n", c.Name, callee, bind)
}

// genMappedWiring emits a map call's fork/callee/merge fan-out. A split-stage
// callee runs through its fork-key-threaded variant (each fork keyed by its
// args-bundle name); other callees take the flattened fork channel directly.
func genMappedWiring(b *strings.Builder, pipeline string, c ir.Call, callee string, prog *ir.Program) {
	fork := forkName(pipeline, c.Name)
	merge := mergeName(pipeline, c.Name)
	fmt.Fprintf(b, "    %s(%s)\n", fork, bindCallArgs(c.Bindings))

	if isSplitStage(prog, c.Callable) {
		fmt.Fprintf(b, "    keyed_%s = %s.out.forks.flatten().map { f -> tuple(f.baseName, f) }\n", c.Name, fork)
		fmt.Fprintf(b, "    out_%s = %s(keyed_%s).map { k, bundle -> bundle }\n", c.Name, callee, c.Name)
	} else {
		fmt.Fprintf(b, "    out_%s = %s(%s.out.forks.flatten())\n", c.Name, callee, fork)
	}

	// ifEmpty([]) ensures MERGE still runs for an empty fork collection
	// (collect() on an empty channel emits nothing), yielding null outputs.
	// FORK.out.keys carries map-fork keys (null for an array fork).
	fmt.Fprintf(b, "    %s(out_%s.collect().ifEmpty([]), %s.out.keys)\n", merge, c.Name, fork)
	// .first() makes the result a value channel so it can feed multiple
	// downstream consumers (non-linear DAGs).
	fmt.Fprintf(b, "    ch_%s = %s.out.first()\n", c.Name, merge)
}

// genDisabledWiring runs the callee only when the resolved `disabled` flag is
// false; disabled forks emit a null outputs bundle instead.
func genDisabledWiring(b *strings.Builder, pipeline string, c ir.Call, callee string) {
	bind := bindName(pipeline, c.Name)
	dis := disableName(pipeline, c.Name)
	nulls := "${projectDir}/nulls/" + qualify(pipeline, c.Name)

	fmt.Fprintf(b, "    %s(%s)\n", dis, bindCallArgs(disableBindings(c)))
	fmt.Fprintf(b, `    g_%[1]s = %[2]s.out.combine(%[3]s.out).branch { a, d ->
        def off = new groovy.json.JsonSlurper().parseText(file("${d}/data.json").text).disabled
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
func bindCallArgs(bindings []ir.Binding) string {
	refs := refCalls(bindings)

	args := make([]string, 0, 1+len(refs))
	args = append(args, "pa")

	for _, id := range refs {
		args = append(args, "ch_"+id)
	}

	return strings.Join(args, ", ")
}

// genPublish emits the PUBLISH process: it reads the entry's final output
// bundle, copies every file-typed output (including nested ones) into the
// results dir under its basename, and writes the published outs JSON. The staged
// bundle is removed before output globbing so only published artifacts remain.
func genPublish(b *strings.Builder, entry string, g genCtx) {
	fmt.Fprintf(b, `process PUBLISH {
  publishDir params.outdir, mode: 'copy'
  input:
    path bundle
  output:
    path '*'
  script:
    """
    %[1]s publish -bundle ${bundle} -dir .%[2]s
    rm -rf ${bundle}
    """
}

`, g.mre, g.producerArgs(entry, types.RoleOut))
}

func genEntry(b *strings.Builder, entryWorkflow string) {
	fmt.Fprintf(b, `workflow {
  pipeargs = Channel.value(file("${projectDir}/entry_args"))
  %[1]s(pipeargs)
  PUBLISH(%[1]s.out)
}
`, entryWorkflow)
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
