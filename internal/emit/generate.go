package emit

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/eunmann/martian-nextflow/internal/ir"
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

// generateStageModule renders a stage's module: its processes and the wf_<stage>
// subworkflow that wraps them.
func generateStageModule(s *ir.Stage, g genCtx) string {
	var b strings.Builder

	b.WriteString("nextflow.enable.dsl=2\n\n")
	genStage(&b, s, g)

	return b.String()
}

// generatePipeModule renders a pipeline's module: includes of its callees (each
// aliased per call so repeated calls get independent instances), the per-call
// helper processes, and the pipeline workflow.
func generatePipeModule(p *ir.Pipeline, prog *ir.Program, g genCtx) string {
	var b strings.Builder

	b.WriteString("nextflow.enable.dsl=2\n\n")
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

	b.WriteString("nextflow.enable.dsl=2\n\n")
	fmt.Fprintf(&b, "include { %s } from './modules/%s'\n\n", export, strings.TrimPrefix(src, "./"))
	genPublish(&b, entryFileParams(prog), g)
	genEntry(&b, export)

	return b.String()
}

// entryFileParams returns the names of the entry callable's file-typed outputs.
func entryFileParams(prog *ir.Program) []string {
	var out []string

	if p, ok := prog.Pipelines[prog.Entry.Callable]; ok {
		for _, param := range p.Out {
			if param.IsFile {
				out = append(out, param.Name)
			}
		}
	}

	if s, ok := prog.Stages[prog.Entry.Callable]; ok {
		for _, param := range s.Out {
			if param.IsFile {
				out = append(out, param.Name)
			}
		}
	}

	return out
}

// genPipeIncludes emits one include per call, aliasing the callee's workflow to
// a per-call name so each call is an independent instance.
func genPipeIncludes(b *strings.Builder, p *ir.Pipeline, prog *ir.Program) {
	for _, c := range p.Calls {
		export, src := calleeModule(prog, c.Callable)
		fmt.Fprintf(b, "include { %s as %s } from '%s'\n", export, callAlias(p.Name, c.Name), src)
	}

	b.WriteString("\n")
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

		genBindProcess(b, bindName(p.Name, c.Name), c.Bindings, g)

		if c.Disabled != nil {
			genDisableProcess(b, p.Name, c, g)
		}
	}

	genBindProcess(b, bindName(p.Name, "return"), p.Returns, g)
}

func genStage(b *strings.Builder, s *ir.Stage, g genCtx) {
	code := g.code[s.Name]
	mainOuts := strings.Join(append(names(s.Out), names(s.ChunkOut)...), ",")
	joinOuts := strings.Join(names(s.Out), ",")
	base := g.stageCmd("main", code, s.Lang)

	if !s.Split {
		genSingleStage(b, s, base, joinOuts)

		return
	}

	genSplitProcesses(b, s, g, base, mainOuts, joinOuts)
	genSplitWorkflow(b, s)
}

func genSingleStage(b *strings.Builder, s *ir.Stage, base, outs string) {
	fmt.Fprintf(b, `process %[1]s {
  cpus %[2]d
  memory '%[3]d GB'
  input:
    path args
  output:
    path "outs__${args.baseName}.json"
  script:
    """
    %[4]s -args ${args} -outs '%[5]s' -threads ${task.cpus} -memgb ${task.memory.toGiga()} -work . -o outs__${args.baseName}.json
    """
}

workflow wf_%[1]s {
  take: args
  main:
    %[1]s(args)
  emit:
    %[1]s.out
}

`, s.Name, cpusOf(s), memOf(s), base, outs)
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
    path 'chunk_*.json', emit: chunks, optional: true
  script:
    """
    %[4]s -args ${args} -work . -o chunks.json -chunkdir .
    """
}

process %[1]s_MAIN {
  cpus %[2]d
  memory '%[3]d GB'
  input:
    tuple path(chunk), path(args)
  output:
    path "out_${chunk.baseName}.json"
  script:
    """
    %[5]s -args ${args} -chunk ${chunk} -outs '%[6]s' -threads ${task.cpus} -memgb ${task.memory.toGiga()} -work . -o out_${chunk.baseName}.json
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
    path 'outs.json'
  script:
    """
    %[7]s -args ${args} -chunkdefs ${defs} -chunkouts "\$(ls -1 out_*.json 2>/dev/null | sort -V | paste -sd, -)" -outs '%[8]s' -work . -o outs.json
    """
}

`, s.Name, cpusOf(s), memOf(s), splitCmd, base, mainOuts, joinCmd, joinOuts)
}

func genSplitWorkflow(b *strings.Builder, s *ir.Stage) {
	fmt.Fprintf(b, `workflow wf_%[1]s {
  take: args
  main:
    a = args.first()
    %[1]s_SPLIT(a)
    main_in = %[1]s_SPLIT.out.chunks.flatten().combine(a)
    %[1]s_MAIN(main_in)
    %[1]s_JOIN(a, %[1]s_SPLIT.out.defs, %[1]s_MAIN.out.collect())
  emit:
    %[1]s_JOIN.out
}

`, s.Name)
}

// genDisableProcess emits a process that resolves a call's `disabled` flag into
// disable.json at runtime.
func genDisableProcess(b *strings.Builder, pipeline string, c ir.Call, g genCtx) {
	block, arg := bindInputs(refCalls(disableBindings(c)))

	fmt.Fprintf(b, `process %[1]s {
  input:
%[2]s  output:
    path 'disable.json'
  script:
    """
    '%[3]s' bind -spec '${projectDir}/bindspecs/%[1]s.json' -pipeargs ${pipeargs}%[4]s -o disable.json
    """
}

`, disableName(pipeline, c.Name), block, g.mre, arg)
}

// bindInputs renders the input block and the -inputs argument for a bind/fork
// process: pipeline args plus one staged file per referenced upstream call.
func bindInputs(refs []string) (string, string) {
	var inputs strings.Builder

	inputs.WriteString("    path pipeargs\n")

	pairs := make([]string, 0, len(refs))
	for _, id := range refs {
		fmt.Fprintf(&inputs, "    path 'in_%s.json'\n", id)
		pairs = append(pairs, fmt.Sprintf("%s=in_%s.json", id, id))
	}

	arg := ""
	if len(pairs) > 0 {
		arg = " -inputs " + strings.Join(pairs, ",")
	}

	return inputs.String(), arg
}

// genBindProcess emits a process that resolves one call's (or the return's)
// input bindings into args.json via `mre bind`.
func genBindProcess(b *strings.Builder, name string, bindings []ir.Binding, g genCtx) {
	block, arg := bindInputs(refCalls(bindings))

	fmt.Fprintf(b, `process %[1]s {
  input:
%[2]s  output:
    path 'args.json'
  script:
    """
    '%[3]s' bind -spec '${projectDir}/bindspecs/%[1]s.json' -pipeargs ${pipeargs}%[4]s -o args.json
    """
}

`, name, block, g.mre, arg)
}

// genForkBindProcess emits a process that resolves a map call's bindings into
// one args file per fork (fork_NNNNN.json) via `mre forkbind`.
func genForkBindProcess(b *strings.Builder, pipeline string, c ir.Call, g genCtx) {
	block, arg := bindInputs(refCalls(c.Bindings))

	fmt.Fprintf(b, `process %[1]s {
  input:
%[2]s  output:
    path 'fork_*.json', optional: true
  script:
    """
    '%[3]s' forkbind -spec '${projectDir}/bindspecs/%[4]s.json' -pipeargs ${pipeargs}%[5]s -chunkdir .
    """
}

`, forkName(pipeline, c.Name), block, g.mre, bindName(pipeline, c.Name), arg)
}

// genMergeProcess emits a process that merges per-fork outputs into the
// map-call result via `mre merge`.
func genMergeProcess(b *strings.Builder, pipeline string, c ir.Call, calleeOuts string, g genCtx) {
	fmt.Fprintf(b, `process %[1]s {
  input:
    path souts
  output:
    path 'merged.json'
  script:
    """
    '%[2]s' merge -outs '%[3]s' -files "\$(ls -1 outs__*.json 2>/dev/null | sort -V | paste -sd, -)" -o merged.json
    """
}

`, mergeName(pipeline, c.Name), g.mre, calleeOuts)
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
func genCallWiring(b *strings.Builder, pipeline string, c ir.Call, _ *ir.Program) {
	callee := callAlias(pipeline, c.Name)

	if c.Mapped {
		fork := forkName(pipeline, c.Name)
		merge := mergeName(pipeline, c.Name)
		fmt.Fprintf(b, "    %s(%s)\n", fork, bindCallArgs(c.Bindings))
		fmt.Fprintf(b, "    out_%s = %s(%s.out.flatten())\n", c.Name, callee, fork)
		// ifEmpty([]) ensures MERGE still runs for an empty fork collection
		// (collect() on an empty channel emits nothing), yielding null outputs.
		fmt.Fprintf(b, "    %s(out_%s.collect().ifEmpty([]))\n", merge, c.Name)
		// .first() makes the result a value channel so it can feed multiple
		// downstream consumers (non-linear DAGs).
		fmt.Fprintf(b, "    ch_%s = %s.out.first()\n", c.Name, merge)

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

// genDisabledWiring runs the callee only when the resolved `disabled` flag is
// false; disabled forks emit a null outputs file instead.
func genDisabledWiring(b *strings.Builder, pipeline string, c ir.Call, callee string) {
	bind := bindName(pipeline, c.Name)
	dis := disableName(pipeline, c.Name)
	nulls := fmt.Sprintf("${projectDir}/nulls/%s__%s.json", pipeline, c.Name)

	fmt.Fprintf(b, "    %s(%s)\n", dis, bindCallArgs(disableBindings(c)))
	fmt.Fprintf(b, `    g_%[1]s = %[2]s.out.combine(%[3]s.out).branch { a, d ->
        def off = new groovy.json.JsonSlurper().parse(d).disabled
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
	return "wf_" + pipeline + "__" + call
}

// disableBindings builds a one-entry binding list for a call's disabled ref.
func disableBindings(c ir.Call) []ir.Binding {
	return []ir.Binding{{Param: "disabled", Ref: c.Disabled}}
}

func disableName(pipeline, call string) string {
	return "DISABLE_" + pipeline + "__" + call
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

// genPublish emits the PUBLISH process. When the entry has file-typed outputs,
// it runs `mre publish` to copy those files into the results dir and rewrite
// their paths to basenames; otherwise it just publishes the outs JSON.
func genPublish(b *strings.Builder, fileParams []string, g genCtx) {
	if len(fileParams) == 0 {
		b.WriteString(`process PUBLISH {
  publishDir params.outdir, mode: 'copy'
  input:
    path 'pipeline_outs.json'
  output:
    path 'pipeline_outs.json'
  script:
    'true'
}

`)

		return
	}

	fmt.Fprintf(b, `process PUBLISH {
  publishDir params.outdir, mode: 'copy'
  input:
    path 'final_outs.json'
  output:
    path '*'
  script:
    """
    %s publish -outs final_outs.json -files '%s' -dir .
    rm -f final_outs.json
    """
}

`, g.mre, strings.Join(fileParams, ","))
}

func genEntry(b *strings.Builder, entryWorkflow string) {
	fmt.Fprintf(b, `workflow {
  pipeargs = Channel.value(file("${projectDir}/entry_args.json"))
  %[1]s(pipeargs)
  PUBLISH(%[1]s.out)
}
`, entryWorkflow)
}

func bindName(pipeline, call string) string {
	return "BIND_" + pipeline + "__" + call
}

func forkName(pipeline, call string) string {
	return "FORK_" + pipeline + "__" + call
}

func mergeName(pipeline, call string) string {
	return "MERGE_" + pipeline + "__" + call
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

// refCalls returns the unique, sorted upstream call ids referenced by bindings.
func refCalls(bindings []ir.Binding) []string {
	seen := map[string]bool{}

	var ids []string

	for _, bnd := range bindings {
		if bnd.Ref != nil && bnd.Ref.Kind == "call" && !seen[bnd.Ref.ID] {
			seen[bnd.Ref.ID] = true

			ids = append(ids, bnd.Ref.ID)
		}
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
