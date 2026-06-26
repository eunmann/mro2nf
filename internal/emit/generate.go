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
	code    map[string]string // stage name -> stage code path
}

// generateNF renders the complete main.nf for a program.
func generateNF(prog *ir.Program, g genCtx) string {
	var b strings.Builder

	b.WriteString("nextflow.enable.dsl=2\n\n")

	for _, name := range sortedKeys(prog.Stages) {
		genStage(&b, prog.Stages[name], g)
	}

	for _, name := range sortedKeys(prog.Pipelines) {
		genPipeline(&b, prog.Pipelines[name], prog, g)
	}

	genPublish(&b)
	genEntry(&b, prog)

	return b.String()
}

func genStage(b *strings.Builder, s *ir.Stage, g genCtx) {
	code := g.code[s.Name]
	mainOuts := strings.Join(append(names(s.Out), names(s.ChunkOut)...), ",")
	joinOuts := strings.Join(names(s.Out), ",")
	base := fmt.Sprintf("%s main -shell %s -stagecode %s -lang %s -call %s -mro %s",
		g.mre, g.shell, code, s.Lang, g.entry, g.mroFile)

	if !s.Split {
		genSingleStage(b, s, g, base, joinOuts)

		return
	}

	genSplitProcesses(b, s, g, base, mainOuts, joinOuts)
	genSplitWorkflow(b, s)
}

func genSingleStage(b *strings.Builder, s *ir.Stage, g genCtx, base, outs string) {
	fmt.Fprintf(b, `process %[1]s {
  cpus %[2]d
  memory '%[3]d GB'
  input:
    path args
  output:
    path 'outs.json'
  script:
    """
    %[4]s -args ${args} -outs '%[5]s' -threads ${task.cpus} -memgb ${task.memory.toGiga()} -work . -o outs.json
    """
}

workflow wf_%[1]s {
  take: args
  main:
    %[1]s(args.first())
  emit:
    %[1]s.out
}

`, s.Name, cpusOf(s), memOf(s), base, outs)
	_ = g
}

func genSplitProcesses(b *strings.Builder, s *ir.Stage, g genCtx, base, mainOuts, joinOuts string) {
	splitCmd := fmt.Sprintf("%s split -shell %s -stagecode %s -lang %s -call %s -mro %s",
		g.mre, g.shell, g.code[s.Name], s.Lang, g.entry, g.mroFile)
	joinCmd := fmt.Sprintf("%s join -shell %s -stagecode %s -lang %s -call %s -mro %s",
		g.mre, g.shell, g.code[s.Name], s.Lang, g.entry, g.mroFile)

	fmt.Fprintf(b, `process %[1]s_SPLIT {
  cpus %[2]d
  memory '%[3]d GB'
  input:
    path args
  output:
    path 'chunks.json', emit: defs
    path 'chunk_*.json', emit: chunks
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
    %[7]s -args ${args} -chunkdefs ${defs} -chunkouts \$(ls -1 out_*.json | sort | paste -sd, -) -outs '%[8]s' -work . -o outs.json
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

func genPipeline(b *strings.Builder, p *ir.Pipeline, prog *ir.Program, g genCtx) {
	for _, c := range p.Calls {
		genBindProcess(b, bindName(p.Name, c.Name), c.Bindings, g)
	}

	genBindProcess(b, bindName(p.Name, "return"), p.Returns, g)
	genPipelineWorkflow(b, p, prog)
}

// genBindProcess emits a process that resolves one call's (or the return's)
// input bindings into args.json via `mre bind`.
func genBindProcess(b *strings.Builder, name string, bindings []ir.Binding, g genCtx) {
	refs := refCalls(bindings)

	var inputs strings.Builder

	inputs.WriteString("    path pipeargs\n")

	pairs := make([]string, 0, len(refs))
	for _, id := range refs {
		fmt.Fprintf(&inputs, "    path 'in_%s.json'\n", id)
		pairs = append(pairs, fmt.Sprintf("%s=in_%s.json", id, id))
	}

	inputsArg := ""
	if len(pairs) > 0 {
		inputsArg = " -inputs " + strings.Join(pairs, ",")
	}

	fmt.Fprintf(b, `process %[1]s {
  input:
%[2]s  output:
    path 'args.json'
  script:
    """
    %[3]s bind -spec ${projectDir}/bindspecs/%[1]s.json -pipeargs ${pipeargs}%[4]s -o args.json
    """
}

`, name, inputs.String(), g.mre, inputsArg)
}

func genPipelineWorkflow(b *strings.Builder, p *ir.Pipeline, prog *ir.Program) {
	var body strings.Builder

	body.WriteString("  main:\n    pa = pipeargs.first()\n")

	for _, c := range p.Calls {
		genCallWiring(&body, p.Name, c)
		_ = prog
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

// genCallWiring emits the BIND + stage-workflow invocation for one call.
func genCallWiring(b *strings.Builder, pipeline string, c ir.Call) {
	bind := bindName(pipeline, c.Name)
	fmt.Fprintf(b, "    %s(%s)\n", bind, bindCallArgs(c.Bindings))
	fmt.Fprintf(b, "    ch_%s = wf_%s(%s.out)\n", c.Name, c.Callable, bind)
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

func genPublish(b *strings.Builder) {
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
}

func genEntry(b *strings.Builder, prog *ir.Program) {
	fmt.Fprintf(b, `workflow {
  pipeargs = Channel.value(file("${projectDir}/entry_args.json"))
  %[1]s(pipeargs)
  PUBLISH(%[1]s.out)
}
`, prog.Entry.Callable)
}

func bindName(pipeline, call string) string {
	return "BIND_" + pipeline + "__" + call
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
