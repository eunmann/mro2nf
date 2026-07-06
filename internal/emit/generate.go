package emit

import (
	"fmt"
	"math"
	"slices"
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
	// runnerBase is the directory holding the -native-runner Python runner
	// (run_stage.py + martian.py): "${projectDir}/_assets/runner" on a shared
	// filesystem, or the baked ctrRunner path for a container backend where the
	// project dir is not mounted into the isolated task.
	runnerBase string
	monitor    bool
	// features holds the opt-in emission toggles (mirrored from Options).
	features featureSet
	code     map[string]string // stage name -> stage code path
	// plan is the whole-program emission plan (per-call kinds + the keyed set),
	// resolved once in buildPlan so the process/wiring/include sites read a fixed
	// decision instead of re-deriving fuseable*/forward/chain predicates.
	plan emitPlan
}

// stageCmd renders a stage-phase invocation, single-quoting every path so
// spaces and shell metacharacters in paths are safe. The default runs the
// phase through `mre` + the Martian adapter (-shell); under -native-runner a
// Python stage instead execs the embedded run_stage.py directly (#79) — no
// martian_shell.py adapter and no mre broker on the stage-execution hop. The
// runner accepts the identical flag tail every call site appends (and the
// -vmemgb/-monitor suffixes below), so this branch is the only emitter change.
func (g genCtx) stageCmd(phase string, s *ir.Stage, vmemExpr string) string {
	cmd := g.stageHead(phase, s)

	// A declared `using(vmem_gb)` is passed so the shim's --monitor caps virtual
	// memory (and reports it in _jobinfo) at the declared value instead of the
	// memory-derived default. A per-chunk __vmem_gb still wins for main (it
	// arrives in the chunk def); a split-returned join override refines it via
	// vmemExpr. When no vmem_gb is declared, vmemExpr is empty and the shim
	// derives vmem from memory as before.
	if vmemExpr != "" {
		cmd += " -vmemgb " + vmemExpr
	}

	if g.monitor {
		cmd += " -monitor"
	}

	return cmd
}

// stageHead renders the phase invocation up to the shared flag tail: the
// direct-call runner for a Python stage under -native-runner, else the
// mre+adapter invocation (with -mrjob for comp stages when configured).
func (g genCtx) stageHead(phase string, s *ir.Stage) string {
	code := g.code[s.Name]

	// A py stage can never carry src args (the Martian compiler rejects them:
	// syntax/compile_stages.go "py stage type cannot have additional
	// arguments"), so the runner path has nothing to forward.
	if g.features.nativeRunner && s.Lang == ir.LangPy {
		// runnerBase deliberately embeds ${projectDir} (a GString ref) so it stays
		// raw; every other baked path is host-literal and goes through gstringLit,
		// so a $ or \ in a stage-code / .mro path reaches bash byte-identical
		// instead of resolving a Groovy variable (MissingPropertyException at task
		// launch, or silently wrong args). runnerBase is the project dir on a shared
		// FS, or the baked ctrRunner in a container image.
		return fmt.Sprintf(`'python3' '%s/run_stage.py' %s -stagecode '%s' -call '%s' -mro '%s'`,
			g.runnerBase, phase, gstringLit(code), gstringLit(g.entry), gstringLit(g.mroFile))
	}

	var cmd strings.Builder

	fmt.Fprintf(&cmd, "'%s' %s -shell '%s' -stagecode '%s' -lang %s -call '%s' -mro '%s'",
		gstringLit(g.mre), phase, gstringLit(g.shell), gstringLit(code), s.Lang, gstringLit(g.entry), gstringLit(g.mroFile))

	// Src args from the stage declaration (`src exec "code.py a b"`) ride to
	// the adapter one -srcarg each (#113). Two quoting layers apply: Groovy
	// interpolates the script GString before bash sees the line, so $ and \
	// must be GString-escaped (gstringLit) or a `$INPUT` arg would resolve a
	// Groovy variable — silently wrong args or a MissingPropertyException.
	// Bash then sees the single-quoted literal; quotes themselves cannot
	// appear (Martian's grammar rejects them in src args).
	for _, a := range s.SrcArgs {
		fmt.Fprintf(&cmd, " -srcarg '%s'", gstringLit(a))
	}

	if g.mrjob != "" {
		fmt.Fprintf(&cmd, " -mrjob '%s'", gstringLit(g.mrjob))
	}

	return cmd.String()
}

// gstringLit escapes a value for literal use inside a process script GString:
// Groovy resolves \-escapes and $-interpolation before bash sees the text, so
// both must be escaped for the value to reach bash byte-identical.
func gstringLit(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)

	return strings.ReplaceAll(s, "$", `\$`)
}

// vmemFlag renders the -vmemgb value for a stage phase from its static
// using(vmem_gb): empty when none is declared (the shim then derives vmem from
// memory). The join phase wraps it so a split-returned join __vmem_gb override
// refines the static default at runtime.
func vmemFlag(s *ir.Stage, phase string) string {
	v := s.Resources.VMemGB
	if v <= 0 {
		return ""
	}

	static := strconv.FormatFloat(v, 'g', -1, 64)
	if phase == "join" {
		return fmt.Sprintf("${(join?.vmem_gb ?: 0) > 0 ? join.vmem_gb : %s}", static)
	}

	return static
}

// producerArgs renders the flags a bundle-producing mre command needs to stage
// the file leaves of callable's params under the given role.
func (g genCtx) producerArgs(callable, role string) string {
	return fmt.Sprintf(" -types 'types.json' -callable '%s' -role %s", callable, role)
}

// bundleOutput renders a producer process's output declaration for a payload
// bundle mre writes into dir `name`: its typed sidecar (data.json) plus its flat
// leaf files (f/L*) as INDIVIDUAL path items, so Nextflow stages each file rather
// than the whole bundle directory (the de-bundle, epic #18 / #13). arity 0..*
// admits a bundle whose payload has no file leaves. mre and the on-disk bundle
// layout are unchanged; only how the bundle crosses a process boundary changes.
func bundleOutput(name string) string {
	return "tuple(" + bundleOutputElems(name) + ")"
}

// bundleOutputEmit is bundleOutput with a named `emit:` label, for a process that
// emits several outputs (Nextflow requires the un-parenthesized tuple form here).
func bundleOutputEmit(name, emit string) string {
	return "tuple " + bundleOutputElems(name) + ", emit: " + emit
}

// bundleOutputElems is bundleOutput's inner path items, for embedding a bundle in
// a larger output tuple (e.g. a keyed tuple(val(key), …)).
func bundleOutputElems(name string) string {
	return fmt.Sprintf("path(\"%[1]s/data.json\"), path(\"%[1]s/f/*\", arity: '0..*')", name)
}

// bundleInput renders a consumer process's input staging for a payload bundle,
// reconstructing the bundle dir `name` (data.json + f/ leaves) from the staged
// sidecar and individual leaf items, so `mre -… name` reads it exactly as before.
func bundleInput(name string) string {
	return "tuple(" + bundleInputElems(name) + ")"
}

// bundleInputElems is bundleInput's inner path items, for embedding a bundle in a
// larger input tuple (e.g. a split MAIN's tuple(val(res), path(chunk), …)).
func bundleInputElems(name string) string {
	return fmt.Sprintf("path('%[1]s/data.json'), path('%[1]s/f/*')", name)
}

// nullBundle renders the channel item for a pre-generated null-output bundle (a
// disabled call's skip result) in the de-bundled tuple shape: its data.json
// sidecar plus an empty leaf list (a null output has no file leaves).
func nullBundle(dir string) string {
	return fmt.Sprintf("tuple(file(\"%s/data.json\"), [])", dir)
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

	genPipeIncludes(&b, p, prog, g)
	genPipeProcesses(&b, p, prog, g)
	genPipelineWorkflow(&b, p, g)

	// The keyed layer (wf_<p>_map + its keyed includes/processes) is only invoked
	// when this pipeline runs under a map call; otherwise it is dead code (#59).
	if g.plan.keyed[p.Name] {
		genKeyedPipeIncludes(&b, p, prog, g)
		genKeyedPipeProcesses(&b, p, prog, g)
		genKeyedPipeline(&b, p, g)
	}

	return b.String()
}

// generateMain renders main.nf: the entry workflow plus PUBLISH. The entry
// callable may be a pipeline or a bare stage.
func generateMain(prog *ir.Program, g genCtx) string {
	var b strings.Builder

	export, src := calleeModule(prog, prog.Entry.Callable)

	fmt.Fprintf(&b, "include { %s } from './modules/%s'\n\n", export, strings.TrimPrefix(src, "./"))
	genPublish(&b, prog, g)
	genEntry(&b, prog, export, g)

	return b.String()
}

// genPipeIncludes emits one include per call, aliasing the callee's workflow to
// a per-call name so each call is an independent instance.
func genPipeIncludes(b *strings.Builder, p *ir.Pipeline, prog *ir.Program, g genCtx) {
	pp := g.plan.pipes[p.Name]

	for _, c := range p.Calls {
		cp := pp.calls[c.Name]

		// A fused non-split / chain call is a self-contained per-call process —
		// no wf_ import.
		if cp.fusedInclude() {
			continue
		}

		// A fused split call imports the stage's MAIN/JOIN phase processes, aliased
		// per call (DSL2 requires an alias since wf_<stage> also invokes them).
		if cp.kind == kindFusedSplit {
			fmt.Fprintf(b, "include { %[1]s_MAIN as %[2]s; %[1]s_JOIN as %[3]s } from './stage_%[1]s.nf'\n",
				cp.stage.Name, fusedMainAlias(p.Name, c.Name), fusedJoinAlias(p.Name, c.Name))

			continue
		}

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

// specFile is the head-node expression that resolves a call's bindspec so
// Nextflow stages it into the task as `spec.json`. Each bind/fork/disable
// process stages only its own spec rather than every call's (see assetsDir).
func specFile(name string) string {
	return `file("${projectDir}/_assets/bindspecs/` + name + `.json")`
}

// genPipeProcesses emits the BIND/FORK/MERGE/DISABLE helper processes for a
// pipeline's calls and its return binding.
func genPipeProcesses(b *strings.Builder, p *ir.Pipeline, prog *ir.Program, g genCtx) {
	pp := g.plan.pipes[p.Name]

	for _, c := range p.Calls {
		cp := pp.calls[c.Name]

		switch cp.kind {
		// A folded chain source (kindFusedAway) emits nothing; a pure forward
		// (kindForward) routes the producer straight in (#14); an always-disabled
		// call (kindFoldedOff, #59 Lever 1) emits only its null output in the
		// wiring — all three need no process.
		case kindFusedAway, kindForward, kindFoldedOff:

		// #59 Lever 4: the chain consumer emits the combined producer+consumer
		// process instead of a plain fused stage.
		case kindFusedChain:
			genFusedChainProcess(b, p.Name, cp.chain, g)

		case kindMapped:
			genMappedProcesses(b, prog, p, c, cp, g)

		// #76: a -native map call scatters in-workflow — a fused forkbind+main
		// process per fork instance, no FORK task. The MERGE gather remains only
		// when it cannot fold into the sole consumer's task (fan-out or an
		// unsupported consumer shape — see mergeFoldable).
		case kindNativeScatter:
			genNativeScatterElementProcess(b, p.Name, c, cp, g)

			if !cp.foldMerge {
				genMergeProcess(b, p.Name, c, calleeOutNames(prog, c.Callable), g)
			}

		// A fuseable non-split leaf (plain or natively-disabled) runs `mre bind`
		// inline in the stage task — the standalone BIND is gone (#16, #59).
		case kindFusedStage, kindFusedDisabled:
			genFusedStageProcess(b, prog, p, c, cp.stage, g)

		// A fuseable split call folds bind into a per-call SPLIT feeding the aliased
		// MAIN/JOIN (#16).
		case kindFusedSplit:
			genFusedSplitProcess(b, p.Name, c, cp.stage, g)
			genFusedSplitWorkflow(b, p.Name, c)

		case kindPlainBind:
			genBindProcess(b, bindName(p.Name, c.Name), c.Bindings, g, prog, p, g.producerArgs(c.Callable, types.RoleIn))

			if cp.disableTask {
				genDisableProcess(b, p.Name, c, g)
			}
		}
	}

	// The return bind builds the pipeline's own output bundle, unless the returns
	// forward one call's outputs verbatim (then no BIND — routed directly), or the
	// native entry LAYOUT folds the return bind inline (#76 — no BIND node here).
	if pp.retFwd == "" && (!g.features.native || p.Name != g.entry) {
		genBindProcess(b, bindName(p.Name, "return"), p.Returns, g, prog, p, g.producerArgs(p.Name, types.RoleOut))
	}
}

func genStage(b *strings.Builder, s *ir.Stage, g genCtx) {
	base := g.stageCmd("main", s, vmemFlag(s, "main"))

	// The fork-keyed variants are only ever invoked for a stage reachable under a
	// map call; for any other stage they are dead process definitions, so emit
	// them only when needed (#59).
	keyed := g.plan.keyed[s.Name]

	if !s.Split {
		genSingleStage(b, s, base, g)

		if keyed {
			genKeyedSingleStage(b, s, base, g)
		}

		return
	}

	genSplitProcesses(b, s, g, base)
	genSplitWorkflow(b, s)

	// A fork-key-threaded variant, used when this split stage is a map-call
	// target so each fork runs its own split/main/join and gathers per fork.
	if keyed {
		genKeyedSplitProcesses(b, s, g, base)
		genKeyedSplitWorkflow(b, s)
	}
}

func genSingleStage(b *strings.Builder, s *ir.Stage, base string, g genCtx) {
	fmt.Fprintf(b, `process %[1]s {
%[2]s
  input:
    %[5]s
    path 'types.json'
  output:
    %[6]s
  script:
    """
    %[3]s -args args%[4]s -threads ${task.cpus} -memgb ${task.memory.toGiga()} -work . -o outs
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

`, s.Name, stageDirectives(s, ""), base, g.producerArgs(s.Name, types.RoleMainOut),
		bundleInput("args"), bundleOutput("outs"))
}

func genSplitProcesses(b *strings.Builder, s *ir.Stage, g genCtx, base string) {
	splitCmd := g.stageCmd("split", s, vmemFlag(s, "split"))
	joinCmd := g.stageCmd("join", s, vmemFlag(s, "join"))

	fmt.Fprintf(b, `process %[1]s_SPLIT {
%[2]s
  input:
    %[11]s
    path 'types.json'
  output:
    path 'chunks.json', emit: defs
    path 'joinres.json', emit: joinres
    path 'chunk_*', emit: chunks, type: 'dir', optional: true
  script:
    """
    %[3]s -args args -work . -o chunks.json -joinres joinres.json -chunkdir . -threads ${task.cpus} -memgb ${task.memory.toGiga()}%[6]s
    """
}

process %[1]s_MAIN {
%[9]s
  input:
    tuple val(res), path(chunk), %[12]s
    path 'types.json'
  output:
    path "out_${chunk.baseName}", type: 'dir'
  script:
    """
    %[4]s -args args -chunk ${chunk}%[7]s -threads ${task.cpus} -memgb ${task.memory.toGiga()} -work . -o out_${chunk.baseName}
    """
}

process %[1]s_JOIN {
%[10]s
  input:
    val join
    %[11]s
    path cdefs
    path souts
    path 'types.json'
  output:
    %[13]s
  script:
    """
    %[5]s -args args -chunkdefs "\$(ls -1d chunk_* 2>/dev/null | sort -V | paste -sd, -)" -chunkouts "\$(ls -1d out_* 2>/dev/null | sort -V | paste -sd, -)"%[8]s -threads ${task.cpus} -memgb ${task.memory.toGiga()} -work . -o outs
    """
}

`, s.Name, stageDirectives(s, ""), splitCmd, base, joinCmd,
		g.producerArgs(s.Name, types.RoleChunkIn),
		g.producerArgs(s.Name, types.RoleMainOut),
		g.producerArgs(s.Name, types.RoleOut),
		stageDirectives(s, "res"), stageDirectives(s, "join"),
		bundleInput("args"), bundleInputElems("args"), bundleOutput("outs"))
}

func genSplitWorkflow(b *strings.Builder, s *ir.Stage) {
	fmt.Fprintf(b, `workflow wf_%[1]s {
  take: args
  main:
    types = file("${projectDir}/_assets/types.json")
    a = args
    %[1]s_SPLIT(a, types)
    chunks = %[1]s_SPLIT.out.chunks.flatten().map { f -> tuple(Mro2nf.chunkRes(f), f) }
    %[1]s_MAIN(chunks.combine(a), types)
    join = %[1]s_SPLIT.out.joinres.map { f -> Mro2nf.parseJson(f) }
    // JOIN's chunk outs (out_chunk_NNNNN) are gathered sorted by name: collect()
    // order is completion order, which is part of JOIN's -resume cache key.
    // toSortedList emits [] on an empty channel, so a 0-chunk split still joins.
    // (x/y, not the a/b used elsewhere: 'a' is a workflow local here.)
    %[1]s_JOIN(join, a, %[1]s_SPLIT.out.chunks.ifEmpty([]), %[1]s_MAIN.out.toSortedList { x, y -> x.name <=> y.name }, types)
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
    '%[3]s' bind -spec 'spec.json' -pipeargs pipeargs%[4]s -o disable
    """
}

`, disableName(pipeline, c.Name), block, g.mre, arg)
}

// bindInputs renders the input block and the -inputs argument for a bind/fork
// process: pipeline args plus one staged bundle per referenced upstream call.
func bindInputs(refs []string) (string, string) {
	// Each input bundle is reconstructed under its own dir (pipeargs/, in_<id>/)
	// from the staged sidecar + individual leaf items. The distinct dir names also
	// keep pipeargs from clobbering this process's own `-o args` output (bug 1).
	return bindInputsHead(bundleInput("pipeargs"), refs)
}

// bindInputsHead is bindInputs with a caller-supplied first input line (e.g.
// the native scatter's key/index-carrying pipeargs tuple). It is foldBindInputs
// with no fold context, so every bind-style input block shares ONE loop and
// staging fixes land everywhere.
func bindInputsHead(head string, refs []string) (string, string) {
	block, arg, _ := foldBindInputs(genCtx{}, nil, nil, head, refs)

	return block, arg
}

// soutsChan and keysChan name a folded producer's gathered-outs and keys
// channels under the given prefix ("ch_" in a workflow body, "ref_" at the
// entry emit boundary). Every site that wires, invokes, or re-exports the pair
// reads these, so the souts-then-keys pairing and spelling cannot drift.
func soutsChan(prefix, id string) string { return prefix + id + "_souts" }

func keysChan(prefix, id string) string { return prefix + id + "_keys" }

// mergeFoldProducer returns the producer call behind ref id when its MERGE
// folds into this consumer's task (#76), or false. A nil pipeline (a caller
// with no fold context, e.g. the plain bindInputs/bindCallArgs wrappers) never
// folds.
func mergeFoldProducer(g genCtx, p *ir.Pipeline, id string) (ir.Call, bool) {
	if p == nil || !g.plan.pipes[p.Name].calls[id].foldMerge {
		return ir.Call{}, false
	}

	return callByName(p, id)
}

// foldBindInputs is bindInputsHead for a fold-aware consumer: a folded-merge
// ref stages the producer's per-fork outs dirs under souts_<id>/ plus its keys
// sidecar instead of a merged bundle, and the returned pre-lines reconstruct
// merged_<id> in-task with the IDENTICAL `mre merge` the MERGE task ran — the
// bundle is byte-identical, it just never crosses a process boundary. The
// souts_<id>/ subdir isolates each producer's outs__* names from every other
// input (including a second folded producer).
func foldBindInputs(g genCtx, prog *ir.Program, p *ir.Pipeline, head string, refs []string) (string, string, string) {
	var inputs, pre strings.Builder

	fmt.Fprintf(&inputs, "    %s\n", head)

	pairs := make([]string, 0, len(refs))

	for _, id := range refs {
		prod, ok := mergeFoldProducer(g, p, id)
		if !ok {
			fmt.Fprintf(&inputs, "    %s\n", bundleInput("in_"+id))
			pairs = append(pairs, fmt.Sprintf("%s=in_%s", id, id))

			continue
		}

		fmt.Fprintf(&inputs, "    path(souts_%[1]s, stageAs: 'souts_%[1]s/*')\n    path 'forkkeys_%[1]s.json'\n", id)
		pairs = append(pairs, fmt.Sprintf("%s=merged_%s", id, id))
		fmt.Fprintf(&pre, "    %s\n", g.mergeCmd(prod.Callable, calleeOutNames(prog, prod.Callable),
			"souts_"+id+"/outs__*", "forkkeys_"+id+".json", "merged_"+id,
			g.plan.pipes[p.Name].calls[prod.Name].emptyNull))
	}

	inputs.WriteString("    path 'types.json'\n")
	inputs.WriteString("    path 'spec.json'\n")

	arg := ""
	if len(pairs) > 0 {
		arg = " -inputs " + strings.Join(pairs, ",")
	}

	return inputs.String(), arg, pre.String()
}

// mergeCmd renders the `mre merge` gather command. The MERGE task, the keyed
// MERGE_K task, and the folded in-task merge (#76) all share it, so the
// invocations cannot drift — they differ only in where the fork outs live and
// where the result lands.
// emptyNull applies mrp's invocation-known-empty rule: zero forks merge to
// null instead of the typed empty (#99).
func (g genCtx) mergeCmd(callable, outs, glob, keysFile, outDir string, emptyNull bool) string {
	flag := ""
	if emptyNull {
		flag = " -emptynull"
	}

	return fmt.Sprintf(`'%s' merge%s -outs '%s' -files "\$(ls -1d %s 2>/dev/null | sort -V | paste -sd, -)" -keysfile %s -o %s%s`,
		g.mre, flag, outs, glob, keysFile, outDir, g.producerArgs(callable, types.RoleOut))
}

// foldCallArgs is bindCallArgsPa for a fold-aware consumer invocation: a
// folded ref feeds the producer's collected per-fork outs and keys channels in
// place of the merged bundle channel, matching foldBindInputs' input order.
func foldCallArgs(g genCtx, p *ir.Pipeline, bindings []ir.Binding, pa, specName string) string {
	refs := refCalls(bindings)

	args := make([]string, 0, len(refs)+1)
	args = append(args, pa)

	for _, id := range refs {
		if _, ok := mergeFoldProducer(g, p, id); ok {
			args = append(args, soutsChan("ch_", id), keysChan("ch_", id))

			continue
		}

		args = append(args, "ch_"+id)
	}

	args = append(args, "types", specFile(specName))

	return strings.Join(args, ", ")
}

// genBindProcess emits a process that resolves one call's (or the return's)
// input bindings into an args bundle via `mre bind`, running any folded
// producer's merge first (#76). prodArgs stages any file leaves of the
// produced bundle (empty for the disable resolver).
func genBindProcess(b *strings.Builder, name string, bindings []ir.Binding, g genCtx, prog *ir.Program, p *ir.Pipeline, prodArgs string) {
	block, arg, pre := foldBindInputs(g, prog, p, bundleInput("pipeargs"), refCalls(bindings))

	fmt.Fprintf(b, `process %[1]s {
  input:
%[2]s  output:
    %[6]s
  script:
    """
%[7]s    '%[3]s' bind -spec 'spec.json' -pipeargs pipeargs%[4]s -o args%[5]s
    """
}

`, name, block, g.mre, arg, prodArgs, bundleOutput("args"), pre)
}

// genForkBindProcess emits a process that resolves a map call's bindings into
// one args bundle per fork (fork_NNNNN/) via `mre forkbind`.
func genForkBindProcess(b *strings.Builder, prog *ir.Program, p *ir.Pipeline, c ir.Call, g genCtx) {
	block, arg, pre := foldBindInputs(g, prog, p, bundleInput("pipeargs"), refCalls(c.Bindings))

	fmt.Fprintf(b, `process %[1]s {
  input:
%[2]s  output:
    path 'fork_*', emit: forks, type: 'dir', optional: true
    path 'forkkeys.json', emit: keys
  script:
    """
%[7]s    '%[3]s' forkbind -spec 'spec.json' -pipeargs pipeargs%[4]s -chunkdir . -mapmode %[6]s%[5]s
    """
}

`, forkName(p.Name, c.Name), block, g.mre, arg, g.producerArgs(c.Callable, types.RoleIn), mapModeArg(c), pre)
}

// mapModeArg is the static fork kind for a map call: "map" for a typed-map (or
// not-statically-resolved "unknown") source, else "array". It drives the
// fork/merge so an empty or null typed source resolves to the typed empty
// ([]/{}) instead of being sniffed from the runtime value (which mis-classifies
// null). "unknown" maps to "map" to stay consistent with forkDims (emit.go),
// whose output-projection treats an unknown mode as a keyed map.
func mapModeArg(c ir.Call) string {
	if c.MapMode == ir.MapModeMap || c.MapMode == ir.MapModeUnknown {
		return ir.MapModeMap
	}

	return ir.MapModeArray
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
    %[3]s
  script:
    """
    %[2]s
    """
}

`, mergeName(pipeline, c.Name),
		g.mergeCmd(c.Callable, calleeOuts, "outs__*", "forkkeys.json", "merged",
			g.plan.pipes[pipeline].calls[c.Name].emptyNull),
		bundleOutput("merged"))
}

func genPipelineWorkflow(b *strings.Builder, p *ir.Pipeline, g genCtx) {
	var body strings.Builder

	// pipeargs is always a value channel (the entry uses Channel.value and nested
	// calls pass the parent's value-channel pa), so it is directly re-readable by
	// every bind — no .first() needed (which would warn as useless on a value).
	body.WriteString("  main:\n    pa = pipeargs\n")
	body.WriteString("    types = file(\"${projectDir}/_assets/types.json\")\n")

	// Preflight gating: wire the gateable preflight calls first (against the raw
	// pipeargs), then gate `pa` on their completion so every other call waits for
	// validation to pass before starting — the early-abort behavior mrp gives a
	// preflight. Only preflights bound solely to pipeline inputs are gates (one
	// bound to another call's output is itself downstream, so it stays in order
	// and cannot gate without a cycle).
	pre, rest := partitionGateablePreflight(p.Calls)
	for _, c := range pre {
		genCallWiring(&body, p, c, g)
	}

	if len(pre) > 0 {
		var gate strings.Builder
		gate.WriteString("ch_" + pre[0].Name)
		for _, c := range pre[1:] {
			gate.WriteString(".combine(ch_" + c.Name + ")")
		}

		// pipeargs is a 2-element bundle tuple (data.json + leaves); after combining
		// the preflight gate channels with it, re-extract those last two elements as
		// the gated pipeargs bundle (not just its final leaf-list element).
		fmt.Fprintf(&body, "    pa = %s.combine(pipeargs).map { tuple(it[-2], it[-1]) }.first()\n", gate.String())
	}

	for _, c := range rest {
		genCallWiring(&body, p, c, g)
	}

	// The pipeline's own output: when its returns forward one call's outputs
	// verbatim, emit that producer's bundle directly instead of rebuilding it in a
	// return BIND (emit-once, #14); otherwise the return bind assembles it. The
	// forward decision is resolved once in the plan (retFwd).
	emit := bindName(p.Name, "return") + ".out"
	if fwd := g.plan.pipes[p.Name].retFwd; fwd != "" {
		emit = "ch_" + fwd
	} else if g.features.native && p.Name == g.entry {
		// #76: the native entry LAYOUT binds the return inline, so emit the return
		// bind's raw inputs (pipeargs + the returned calls) instead of running a
		// standalone BIND_<entry>__return.
		var em strings.Builder
		em.WriteString("pargs = pa")

		for _, id := range refCalls(p.Returns) {
			// A folded producer hands LAYOUT its per-fork outs + keys channels; the
			// merge runs inline in LAYOUT before the return bind (#76).
			if _, ok := mergeFoldProducer(g, p, id); ok {
				fmt.Fprintf(&em, "\n    %s = %s\n    %s = %s",
					soutsChan("ref_", id), soutsChan("ch_", id), keysChan("ref_", id), keysChan("ch_", id))

				continue
			}

			fmt.Fprintf(&em, "\n    ref_%s = ch_%s", id, id)
		}

		emit = em.String()
	} else {
		fmt.Fprintf(&body, "    %s(%s)\n", bindName(p.Name, "return"), foldCallArgs(g, p, p.Returns, "pa", bindName(p.Name, "return")))
	}

	fmt.Fprintf(b, `workflow %s {
  take: pipeargs
%s  emit:
    %s
}

`, p.Name, body.String(), emit)
}

// partitionGateablePreflight splits calls into the gateable preflight calls
// (preflight, plain — not mapped/disabled — and bound only to pipeline inputs
// or literals; the condition is preflightUngateable, shared with the Warnings
// wording) and everything else, each in original order. A gateable preflight
// depends on nothing but pipeargs, so it can run first and gate the rest without
// a cycle. A preflight that references another call is left in place (it cannot
// gate the pipeline it is downstream of) and keeps its prior in-order behavior.
func partitionGateablePreflight(calls []ir.Call) ([]ir.Call, []ir.Call) {
	var pre, rest []ir.Call

	for _, c := range calls {
		if c.Preflight && preflightUngateable(c) == "" {
			pre = append(pre, c)
		} else {
			rest = append(rest, c)
		}
	}

	return pre, rest
}

// bindingsRefCall reports whether any binding value references another call's
// output (Ref kind "call"), recursing into array/object literals.
func bindingsRefCall(bindings []ir.Binding) bool {
	var refsCall func(ir.Value) bool
	refsCall = func(v ir.Value) bool {
		switch {
		case v.Ref != nil:
			return v.Ref.Kind == ir.RefKindCall
		case v.Array != nil:
			return slices.ContainsFunc(v.Array, refsCall)
		case v.Object != nil:
			for _, e := range v.Object {
				if refsCall(e) {
					return true
				}
			}
		}

		return false
	}

	return slices.ContainsFunc(bindings, func(bnd ir.Binding) bool {
		return refsCall(bnd.Value)
	})
}

// genCallWiring emits the wiring for one call: a map-call fork/merge fan-out, a
// disabled-aware branch, an emit-once forward, or a plain BIND + callee invocation.
func genCallWiring(b *strings.Builder, p *ir.Pipeline, c ir.Call, g genCtx) {
	pipeline := p.Name
	callee := callAlias(pipeline, c.Name)
	cp := g.plan.pipes[pipeline].calls[c.Name]

	switch cp.kind {
	// #59 Lever 4: a source folded into its consumer emits no wiring of its own.
	case kindFusedAway:

	// #59 Lever 1: an always-disabled call is pruned to its pre-generated null
	// output bundle — downstream reads it exactly as it would a runtime skip.
	case kindFoldedOff:
		fmt.Fprintf(b, "    ch_%s = Channel.value(%s)\n", c.Name,
			nullBundle("${projectDir}/nulls/"+qualify(pipeline, c.Name)))

	// The chain consumer runs the combined producer+consumer process off pipeargs.
	case kindFusedChain:
		var specs strings.Builder
		for _, l := range cp.chain {
			fmt.Fprintf(&specs, ", %s", specFile(bindName(pipeline, l.call.Name)))
		}
		fmt.Fprintf(b, "    ch_%s = %s(pa, types%s)\n", c.Name, fusedName(pipeline, c.Name), specs.String())

	case kindMapped:
		genMappedWiring(b, p, c, callee, g)

	// #76: a -native map call scatters in-workflow off pipeargs; no FORK task.
	case kindNativeScatter:
		genNativeScatterWiring(b, pipeline, c, cp)

	// Emit-once routing (#18/#14): feed the forwarded producer's value channel
	// straight into the callee, skipping the BIND that would re-materialize files.
	case kindForward:
		fmt.Fprintf(b, "    ch_%s = %s(ch_%s)\n", c.Name, callee, cp.fwd)

	// A fuseable non-split stage invokes its fused bind+main process directly (#16).
	case kindFusedStage:
		fmt.Fprintf(b, "    ch_%s = %s(%s)\n", c.Name, fusedName(pipeline, c.Name),
			foldCallArgs(g, p, c.Bindings, "pa", bindName(pipeline, c.Name)))

	// A fuseable split call invokes its per-call fused workflow (bind+split → MAIN
	// → JOIN); types and bindspec are resolved inside it (#16).
	case kindFusedSplit:
		fmt.Fprintf(b, "    ch_%s = %s(%s)\n", c.Name, fusedName(pipeline, c.Name),
			fusedSplitCallArgs(c.Bindings))

	// A natively-disabled leaf fuses bind into the fused process (#59, Lever 3).
	case kindFusedDisabled:
		genFusedDisabledWiring(b, p, c, g)

	case kindPlainBind:
		bind := bindName(pipeline, c.Name)
		fmt.Fprintf(b, "    %s(%s)\n", bind, foldCallArgs(g, p, c.Bindings, "pa", bind))

		if c.Disabled != nil {
			genDisabledWiring(b, pipeline, c, callee)
		} else {
			// Bind outputs are value channels (every input traces back to the
			// Channel.value entry args), so the callee result is itself a reusable
			// value channel — no .first() needed for multiple consumers.
			fmt.Fprintf(b, "    ch_%s = %s(%s.out)\n", c.Name, callee, bind)
		}
	}
}

// forwardProducer reports the single upstream call id whose ENTIRE output bundle
// a set of bindings forwards verbatim, or ("", false). Every binding must be a
// name-preserving whole-field reference (`X = PROD.X`) to the SAME plain, clean
// upstream call, AND the forwarded set must be EXACTLY that producer's declared
// outputs — no fewer (a subset would route extra fields/files a projecting BIND
// drops, staging leaves the consumer never needs) and, since Martian binds every
// input, no more. Then the consumer's args ARE the producer's output bundle and
// the BIND is pure plumbing, routable straight through (emit-once, #14).
func forwardProducer(bindings []ir.Binding, p *ir.Pipeline, prog *ir.Program) (string, bool) {
	if len(bindings) == 0 {
		return "", false
	}

	id := ""
	forwarded := make(map[string]bool, len(bindings))

	for _, bnd := range bindings {
		r := bnd.Value.Ref
		if r == nil || r.Kind != ir.RefKindCall || r.Output != bnd.Param || strings.Contains(r.Output, ".") {
			return "", false
		}

		if id == "" {
			id = r.ID
		} else if id != r.ID {
			return "", false
		}

		forwarded[bnd.Param] = true
	}

	prod := findCall(p, id)
	// A clean, plain producer only: not mapped (a fork map/array wrap) and not
	// disabled (a possibly null bundle of a different shape).
	if prod == nil || prod.Mapped || prod.Disabled != nil {
		return "", false
	}

	// Exact coverage: the producer's declared outputs must be exactly the
	// forwarded set, so routing its whole bundle adds no extra fields/files.
	outs := calleeOutParams(prog, p, id)
	if len(outs) != len(forwarded) {
		return "", false
	}

	for _, o := range outs {
		if !forwarded[o.Name] {
			return "", false
		}
	}

	return id, true
}

// callForwardProducer applies forwardProducer to a plain (non-mapped, enabled,
// non-preflight) call's input bindings.
func callForwardProducer(c ir.Call, p *ir.Pipeline, prog *ir.Program) (string, bool) {
	if c.Mapped || c.Disabled != nil || c.Preflight {
		return "", false
	}

	return forwardProducer(c.Bindings, p, prog)
}

// fuseableStageCall reports the stage a plain call resolves its bindings into
// inline, or (nil, false). A non-split stage call whose bind is a genuine payload
// transform (not a pure forward, which #14 routes with no process at all) can run
// `mre bind` at the head of the SAME task as its `main` phase — no standalone
// BIND_* task, and its referenced files stage into the one task once instead of
// being staged to the bind and re-staged to the stage (fold BIND, #16). Mapped,
// disabled, split-stage, and sub-pipeline callees keep their BIND. A preflight
// leaf stage fuses too (#59): its output only signals completion to the pa gate,
// and the fused process is identical to a normal stage — preflight semantics live
// entirely in the gate wiring (partitionGateablePreflight), not the process.
func fuseableStageCall(c ir.Call, p *ir.Pipeline, prog *ir.Program) (*ir.Stage, bool) {
	if c.Mapped || c.Disabled != nil {
		return nil, false
	}

	if _, ok := callForwardProducer(c, p, prog); ok {
		return nil, false
	}

	s, ok := prog.Stages[c.Callable]
	if !ok || s.Split {
		return nil, false
	}

	return s, true
}

// chainFusion returns the maximal linear run of leaf stages ending at c that
// -fuse-chains folds into one task (#59 Lever 4, extended to N stages by #81):
// source-first, each stage feeding only the next, all equal-resource fuseable
// leaves, the first a source with no call inputs. It walks backward from c,
// prepending a producer while it is a single-consumer, equal-resource fuseable
// leaf; the run must reach a source so no stage needs an external channel (the
// fused task takes only pipeargs). Returns nil unless the chain has >= 2 stages.
func chainFusion(c ir.Call, p *ir.Pipeline, prog *ir.Program, fuseChains bool) ([]chainLink, bool) {
	if !fuseChains {
		return nil, false
	}

	cs, ok := chainConsumer(c, prog)
	if !ok {
		return nil, false
	}

	chain := []chainLink{{call: c, stage: cs}}
	cur, curStage := c, cs

	for {
		refs := refCalls(cur.Bindings)
		if len(refs) == 0 {
			break // reached a source — the chain is complete
		}

		if len(refs) != 1 {
			return nil, false // a stage with two call inputs can't be a clean link
		}

		prod, ok := callByName(p, refs[0])
		if !ok || consumerCount(prod.Name, p) != 1 {
			return nil, false // shared producer — the chain must start at a source
		}

		ps, ok := chainConsumer(prod, prog)
		if !ok || ps.Resources != curStage.Resources {
			return nil, false
		}

		chain = append([]chainLink{{call: prod, stage: ps}}, chain...)
		cur, curStage = prod, ps
	}

	// A fusion needs at least a producer and a consumer.
	const minChain = 2
	if len(chain) < minChain {
		return nil, false
	}

	return chain, true
}

// chainConsumer reports whether c can be a link in a fused chain: a non-split
// leaf stage that is not mapped/disabled/preflight. It permits a pure-forward
// stage (#73) — its bind is a forward the fused process runs inline — so a source
// feeding a forwarding stage folds too.
func chainConsumer(c ir.Call, prog *ir.Program) (*ir.Stage, bool) {
	if c.Mapped || c.Disabled != nil || c.Preflight {
		return nil, false
	}

	s, ok := prog.Stages[c.Callable]
	if !ok || s.Split {
		return nil, false
	}

	return s, true
}

// callByName returns the pipeline call with the given instance id.
func callByName(p *ir.Pipeline, name string) (ir.Call, bool) {
	for _, c := range p.Calls {
		if c.Name == name {
			return c, true
		}
	}

	return ir.Call{}, false
}

// consumerCount counts the call inputs and pipeline-return references that read
// the named call's output — how many places depend on it.
func consumerCount(name string, p *ir.Pipeline) int {
	n := 0

	for _, c := range p.Calls {
		for _, r := range refCalls(c.Bindings) {
			if r == name {
				n++
			}
		}

		// A disable ref (using(disabled = <call>.flag)) lives on the call, not in
		// its bindings, but still depends on the producer's output — so it counts
		// as a consumer (else the producer would be folded away and the gate's
		// ch_<producer> would dangle).
		if c.Disabled != nil && c.Disabled.Kind == ir.RefKindCall && c.Disabled.ID == name {
			n++
		}
	}

	for _, r := range refCalls(p.Returns) {
		if r == name {
			n++
		}
	}

	return n
}

// fusedAwayProducers is the set of source calls folded into a downstream
// consumer by -fuse-chains, which must therefore not be emitted or wired on
// their own.
func fusedAwayProducers(p *ir.Pipeline, prog *ir.Program, fuseChains bool) map[string]bool {
	if !fuseChains {
		return nil
	}

	away := map[string]bool{}

	for _, c := range p.Calls {
		if chain, ok := chainFusion(c, p, prog, fuseChains); ok {
			for _, l := range chain[:len(chain)-1] {
				away[l.call.Name] = true
			}
		}
	}

	return away
}

// genFusedChainProcess emits one process running producer then consumer inline:
// bind+main for the source, then bind+main for the consumer with the source's
// outputs fed in locally (#59, Lever 4). Both use the consumer's directives (the
// resources are equal, per chainFusion).
func genFusedChainProcess(b *strings.Builder, pipeline string, chain []chainLink, g genCtx) {
	last := chain[len(chain)-1]

	var specInputs, script strings.Builder

	for i, link := range chain {
		fmt.Fprintf(&specInputs, "    path 'spec_%d.json'\n", i)

		base := g.stageCmd("main", link.stage, vmemFlag(link.stage, "main"))

		inputs := ""
		if i > 0 {
			inputs = fmt.Sprintf(" -inputs %s=outs_%d", chain[i-1].call.Name, i-1)
		}

		outVar := fmt.Sprintf("outs_%d", i)
		if i == len(chain)-1 {
			outVar = "outs"
		}

		fmt.Fprintf(&script, "    '%s' bind -spec 'spec_%d.json' -pipeargs pipeargs%s -o args_%d%s\n",
			g.mre, i, inputs, i, g.producerArgs(link.call.Callable, types.RoleIn))
		fmt.Fprintf(&script, "    %s -args args_%d%s -threads ${task.cpus} -memgb ${task.memory.toGiga()} -work . -o %s\n",
			base, i, g.producerArgs(link.call.Callable, types.RoleMainOut), outVar)
	}

	fmt.Fprintf(b, `process %[1]s {
%[2]s
  input:
    %[3]s
    path 'types.json'
%[4]s  output:
    %[5]s
  script:
    """
%[6]s    """
}

`, fusedName(pipeline, last.call.Name), stageDirectives(last.stage, ""), bundleInput("pipeargs"),
		specInputs.String(), bundleOutput("outs"), script.String())
}

// fuseableDisabledStage reports a non-split leaf-stage call whose disable is
// gated natively (#59, Lever 2): because the run/skip decision is read on the
// driver — not from a resolved DISABLE bundle — the call's own bind no longer has
// to run before the gate, so it fuses into the stage task like an un-disabled
// leaf call. The enabled branch feeds the fused bind+main process the fork's
// pipeline args; the skipped branch yields the null bundle (#59, Lever 3).
func fuseableDisabledStage(c ir.Call, p *ir.Pipeline, prog *ir.Program) (*ir.Stage, bool) {
	if c.Mapped || c.Disabled == nil || c.Preflight {
		return nil, false
	}

	if _, _, ok := nativeDisableGate(c); !ok {
		return nil, false
	}

	if _, ok := callForwardProducer(c, p, prog); ok {
		return nil, false
	}

	s, ok := prog.Stages[c.Callable]
	if !ok || s.Split {
		return nil, false
	}

	return s, true
}

func fusedName(pipeline, call string) string {
	return "STAGE_" + qualify(pipeline, call)
}

// The fused split emits a per-call bind+split process plus per-call aliased
// imports of the stage's MAIN/JOIN phase processes (DSL2 requires an alias since
// the plain wf_<stage> also invokes them), wired by a per-call fused workflow
// named fusedName.
func fusedSplitProc(pipeline, call string) string { return fusedName(pipeline, call) + "_SP" }
func fusedMainAlias(pipeline, call string) string { return fusedName(pipeline, call) + "_MN" }
func fusedJoinAlias(pipeline, call string) string { return fusedName(pipeline, call) + "_JN" }

// fuseableSplitCall reports the split stage a plain call folds its BIND into, or
// (nil, false). Like fuseableStageCall but for split stages: the fold runs
// `mre bind` at the head of the SPLIT task, which then emits the bound args for
// the (aliased) MAIN/JOIN — removing the standalone BIND (#16).
func fuseableSplitCall(c ir.Call, p *ir.Pipeline, prog *ir.Program) (*ir.Stage, bool) {
	if c.Mapped || c.Disabled != nil || c.Preflight {
		return nil, false
	}

	if _, ok := callForwardProducer(c, p, prog); ok {
		return nil, false
	}

	s, ok := prog.Stages[c.Callable]
	if !ok || !s.Split {
		return nil, false
	}

	return s, true
}

// fusedSplitCallArgs renders the fused split workflow's actual arguments: the
// pipeline args plus each referenced upstream call's channel (types and the
// bindspec are resolved inside the workflow, not passed).
func fusedSplitCallArgs(bindings []ir.Binding) string {
	refs := refCalls(bindings)
	args := make([]string, 0, len(refs)+1)
	args = append(args, "pa")

	for _, id := range refs {
		args = append(args, "ch_"+id)
	}

	return strings.Join(args, ", ")
}

// genFusedSplitProcess emits the per-call bind+split process: it resolves the
// call's inputs into a local args bundle (staging referenced files into this one
// task) and runs the split phase, emitting the bound args alongside the chunk
// defs/resources so the aliased MAIN/JOIN can consume them.
func genFusedSplitProcess(b *strings.Builder, pipeline string, c ir.Call, s *ir.Stage, g genCtx) {
	block, arg := bindInputs(refCalls(c.Bindings))
	splitCmd := g.stageCmd("split", s, vmemFlag(s, "split"))

	fmt.Fprintf(b, `process %[1]s {
%[2]s
  input:
%[3]s  output:
    %[7]s
    path 'chunks.json', emit: defs
    path 'joinres.json', emit: joinres
    path 'chunk_*', emit: chunks, type: 'dir', optional: true
  script:
    """
    '%[4]s' bind -spec 'spec.json' -pipeargs pipeargs%[5]s -o args%[8]s
    %[6]s -args args -work . -o chunks.json -joinres joinres.json -chunkdir . -threads ${task.cpus} -memgb ${task.memory.toGiga()}%[9]s
    """
}

`, fusedSplitProc(pipeline, c.Name), stageDirectives(s, ""), block, g.mre, arg, splitCmd,
		bundleOutputEmit("args", "args"), g.producerArgs(c.Callable, types.RoleIn),
		g.producerArgs(s.Name, types.RoleChunkIn))
}

// genFusedSplitWorkflow emits the per-call workflow wiring the fused SPLIT to the
// aliased MAIN/JOIN, mirroring genSplitWorkflow but sourcing the bound args from
// the fused SPLIT's `args` output (made re-readable with .first()).
func genFusedSplitWorkflow(b *strings.Builder, pipeline string, c ir.Call) {
	var take strings.Builder
	take.WriteString("    pipeargs\n")

	refs := refCalls(c.Bindings)
	refArgs := make([]string, 0, len(refs))

	for _, id := range refs {
		v := "r_" + id
		fmt.Fprintf(&take, "    %s\n", v)
		refArgs = append(refArgs, v)
	}

	// The fused SPLIT is called with pipeargs, one channel per ref, then the
	// broadcast types manifest and this call's bindspec.
	callArgs := append(append([]string{"pipeargs"}, refArgs...), "types", "spec")

	fmt.Fprintf(b, `workflow %[1]s {
  take:
%[2]s  main:
    types = file("${projectDir}/_assets/types.json")
    spec = %[6]s
    %[3]s(%[7]s)
    a = %[3]s.out.args.first()
    chunks = %[3]s.out.chunks.flatten().map { f -> tuple(Mro2nf.chunkRes(f), f) }
    %[4]s(chunks.combine(a), types)
    join = %[3]s.out.joinres.map { f -> Mro2nf.parseJson(f) }
    // Sorted for the same -resume cache-key reason as genSplitWorkflow's JOIN.
    %[5]s(join, a, %[3]s.out.chunks.ifEmpty([]), %[4]s.out.toSortedList { x, y -> x.name <=> y.name }, types)
  emit:
    %[5]s.out
}

`, fusedName(pipeline, c.Name), take.String(), fusedSplitProc(pipeline, c.Name),
		fusedMainAlias(pipeline, c.Name), fusedJoinAlias(pipeline, c.Name),
		specFile(bindName(pipeline, c.Name)), strings.Join(callArgs, ", "))
}

// genFusedStageProcess emits a per-call process that runs `mre bind` then the
// stage's `main` phase in one task: the bind resolves the call's inputs into a
// local args bundle (staging its referenced files into this task once), and main
// consumes it — folding the standalone BIND away (#16). A folded producer's
// merge runs first in the same task (#76).
func genFusedStageProcess(b *strings.Builder, prog *ir.Program, p *ir.Pipeline, c ir.Call, s *ir.Stage, g genCtx) {
	block, arg, pre := foldBindInputs(g, prog, p, bundleInput("pipeargs"), refCalls(c.Bindings))
	base := g.stageCmd("main", s, vmemFlag(s, "main"))

	fmt.Fprintf(b, `process %[1]s {
%[2]s
  input:
%[3]s  output:
    %[7]s
  script:
    """
%[10]s    '%[4]s' bind -spec 'spec.json' -pipeargs pipeargs%[5]s -o args%[8]s
    %[6]s -args args%[9]s -threads ${task.cpus} -memgb ${task.memory.toGiga()} -work . -o outs
    """
}

`, fusedName(p.Name, c.Name), stageDirectives(s, ""), block, g.mre, arg, base,
		bundleOutput("outs"), g.producerArgs(c.Callable, types.RoleIn),
		g.producerArgs(c.Callable, types.RoleMainOut), pre)
}

// genMappedProcesses emits the processes a kindMapped call needs: the FORK
// resolve (one task, O(total)), the MERGE gather unless it folds into the sole
// consumer (#99), and a keyed DISABLE where the flag is not a driver-readable
// field.
func genMappedProcesses(b *strings.Builder, prog *ir.Program, p *ir.Pipeline, c ir.Call, cp callPlan, g genCtx) {
	genForkBindProcess(b, prog, p, c, g)

	if !cp.foldMerge {
		genMergeProcess(b, p.Name, c, calleeOutNames(prog, c.Callable), g)
	}

	// A natively-gated disable reads the flag on the driver, so no DISABLE
	// process; a keyed pipeline still emits its keyed DISABLE separately.
	if cp.disableTask {
		genDisableProcess(b, p.Name, c, g)
	}
}

// genMappedWiring emits a map call's fork/callee/merge fan-out. A split-stage
// callee runs through its fork-key-threaded variant (each fork keyed by its
// args-bundle name); other callees take the flattened fork channel directly.
func genMappedWiring(b *strings.Builder, p *ir.Pipeline, c ir.Call, callee string, g genCtx) {
	pipeline := p.Name
	fork := forkName(pipeline, c.Name)
	merge := mergeName(pipeline, c.Name)

	// When the map call is disabled, gate the fork's pipeline-args by the runtime
	// flag so the forks only run when enabled; on skip the call yields its null
	// outputs bundle instead.
	// The FORK process reuses the call's bind spec (see genForkBindProcess).
	forkArgs := foldCallArgs(g, p, c.Bindings, "pa", bindName(pipeline, c.Name))
	if c.Disabled != nil {
		forkArgs = genMappedDisableGate(b, p, c, g)
	}

	fmt.Fprintf(b, "    %s(%s)\n", fork, forkArgs)
	// Each fork is keyed by its args-bundle name and run through the callee's
	// fork-key-threaded variant, which emits tuple(key, outBundle).
	fmt.Fprintf(b, "    keyed_%s = %s.out.forks.flatten().map { f -> tuple(f.baseName, f) }\n", c.Name, fork)
	fmt.Fprintf(b, "    out_%s = %s(keyed_%s).map { k, bundle -> bundle }\n", c.Name, callee, c.Name)

	// A folded merge (#99) runs inside the sole consumer's task: expose the
	// gathered outs__<key> bundles (sorted by name, so the consumer's input
	// ORDER is deterministic across runs and -resume-stable) and the keys
	// sidecar as the fold-contract channel pair instead of a MERGE task.
	// toSortedList emits [] for zero forks; dormancy rests on the keys channel
	// (FORK never ran -> no keys item), exactly as the scatter fold.
	if g.plan.pipes[pipeline].calls[c.Name].foldMerge {
		fmt.Fprintf(b, "    %s = out_%s.toSortedList { a, b -> a.name <=> b.name }\n", soutsChan("ch_", c.Name), c.Name)
		fmt.Fprintf(b, "    %s = %s.out.keys.first()\n", keysChan("ch_", c.Name), fork)

		return
	}

	// The fork outs are gathered sorted by bundle name: collect() order is
	// completion order, which is part of MERGE's -resume cache key. toSortedList
	// emits [] on an empty channel exactly where the ifEmpty([]) it replaces did:
	// for an empty fork collection MERGE still runs and yields the typed empty
	// ([] for an array fork, {} for a map fork), and on a disabled skip the []
	// emission is inert — dormancy rests on FORK.out.keys (FORK never ran, so no
	// keys item and MERGE never fires), as on the foldMerge path above.
	// FORK.out.keys carries map-fork keys (null for an array fork).
	fmt.Fprintf(b, "    %s(out_%s.toSortedList { x, y -> x.name <=> y.name }, %s.out.keys, types)\n", merge, c.Name, fork)

	if c.Disabled != nil {
		// On the disabled branch FORK is skipped, so MERGE produces nothing; mix
		// in the null bundle. .first() makes the merged result reusable.
		nulls := "${projectDir}/nulls/" + qualify(pipeline, c.Name)
		// The skip item's arity varies with the gate (native reads add 0/2 extra
		// tuple slots), and the null bundle ignores it — so accept any shape.
		fmt.Fprintf(b, "    s_%[1]s = g_%[1]s.skip.map { %[2]s }\n", c.Name, nullBundle(nulls))
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
func genMappedDisableGate(b *strings.Builder, p *ir.Pipeline, c ir.Call, g genCtx) string {
	pipeline := p.Name
	bind := bindName(pipeline, c.Name)

	// Native gating (#59, Lever 2): read the flag directly instead of a DISABLE
	// task. For self.<field> the flag is in the fork's own pipeline args (pa); for
	// CALL.out.<field> it is in the upstream channel.
	if src, field, ok := nativeDisableGate(c); ok {
		if src == "pa" {
			fmt.Fprintf(b, `    g_%[1]s = pa.branch { data, leaves ->
        def off = Mro2nf.disabledField(data, '%[2]s')
        run: !off
        skip: off
    }
`, c.Name, field)

			return foldCallArgs(g, p, c.Bindings, fmt.Sprintf("g_%s.run.map { data, leaves -> tuple(data, leaves) }", c.Name), bind)
		}

		fmt.Fprintf(b, `    g_%[1]s = pa.combine(%[2]s).branch { data, leaves, gd, gl ->
        def off = Mro2nf.disabledField(gd, '%[3]s')
        run: !off
        skip: off
    }
`, c.Name, src, field)

		return foldCallArgs(g, p, c.Bindings, fmt.Sprintf("g_%s.run.map { data, leaves, gd, gl -> tuple(data, leaves) }", c.Name), bind)
	}

	dis := disableName(pipeline, c.Name)
	fmt.Fprintf(b, "    %s(%s)\n", dis, bindCallArgs(disableBindings(c), dis))
	fmt.Fprintf(b, `    g_%[1]s = pa.combine(%[2]s.out).branch { data, leaves, d ->
        def off = Mro2nf.disabled(d)
        run: !off
        skip: off
    }
`, c.Name, dis)

	// Feeds the FORK process, which reuses the call's bind spec.
	return foldCallArgs(g, p, c.Bindings, fmt.Sprintf("g_%s.run.map { data, leaves, d -> tuple(data, leaves) }", c.Name), bind)
}

// genDisabledWiring runs the callee only when the resolved `disabled` flag is
// false; disabled forks emit a null outputs bundle instead.
func genDisabledWiring(b *strings.Builder, pipeline string, c ir.Call, callee string) {
	bind := bindName(pipeline, c.Name)
	nulls := "${projectDir}/nulls/" + qualify(pipeline, c.Name)

	// When the disable flag resolves to a single top-level field of the pipeline
	// args or an upstream output, read it natively on the driver instead of
	// spending a whole DISABLE task on one `mre bind` (#59, Lever 2).
	if src, field, ok := nativeDisableGate(c); ok {
		fmt.Fprintf(b, `    g_%[1]s = %[2]s.out.combine(%[3]s).branch { data, leaves, gd, gl ->
        def off = Mro2nf.disabledField(gd, '%[4]s')
        run: !off
        skip: off
    }
    r_%[1]s = %[5]s(g_%[1]s.run.map { data, leaves, gd, gl -> tuple(data, leaves) })
    s_%[1]s = g_%[1]s.skip.map { data, leaves, gd, gl -> %[6]s }
    ch_%[1]s = r_%[1]s.mix(s_%[1]s).first()
`, c.Name, bind, src, field, callee, nullBundle(nulls))

		return
	}

	dis := disableName(pipeline, c.Name)

	fmt.Fprintf(b, "    %s(%s)\n", dis, bindCallArgs(disableBindings(c), dis))
	fmt.Fprintf(b, `    g_%[1]s = %[2]s.out.combine(%[3]s.out).branch { data, leaves, d ->
        def off = Mro2nf.disabled(d)
        run: !off
        skip: off
    }
    r_%[1]s = %[4]s(g_%[1]s.run.map { data, leaves, d -> tuple(data, leaves) })
    s_%[1]s = g_%[1]s.skip.map { data, leaves, d -> %[5]s }
    ch_%[1]s = r_%[1]s.mix(s_%[1]s).first()
`, c.Name, bind, dis, callee, nullBundle(nulls))
}

// genFusedDisabledWiring gates a natively-disabled leaf-stage call and feeds the
// enabled forks straight into the fused bind+main process — no standalone BIND
// (#59, Lever 3). The self case branches on pa (the flag lives in the fork args);
// the upstream-ref case combines pa with the producing channel to read the flag.
func genFusedDisabledWiring(b *strings.Builder, p *ir.Pipeline, c ir.Call, g genCtx) {
	pipeline := p.Name
	bind := bindName(pipeline, c.Name)
	fused := fusedName(pipeline, c.Name)
	nulls := "${projectDir}/nulls/" + qualify(pipeline, c.Name)
	src, field, _ := nativeDisableGate(c)

	if src == "pa" {
		enabled := fmt.Sprintf("g_%s.run", c.Name)
		fmt.Fprintf(b, `    g_%[1]s = pa.branch { data, leaves ->
        def off = Mro2nf.disabledField(data, '%[2]s')
        run: !off
        skip: off
    }
    r_%[1]s = %[3]s(%[4]s)
    s_%[1]s = g_%[1]s.skip.map { data, leaves -> %[5]s }
    ch_%[1]s = r_%[1]s.mix(s_%[1]s).first()
`, c.Name, field, fused, foldCallArgs(g, p, c.Bindings, enabled, bind), nullBundle(nulls))

		return
	}

	enabled := fmt.Sprintf("g_%s.run.map { data, leaves, gd, gl -> tuple(data, leaves) }", c.Name)
	fmt.Fprintf(b, `    g_%[1]s = pa.combine(%[2]s).branch { data, leaves, gd, gl ->
        def off = Mro2nf.disabledField(gd, '%[3]s')
        run: !off
        skip: off
    }
    r_%[1]s = %[4]s(%[5]s)
    s_%[1]s = g_%[1]s.skip.map { data, leaves, gd, gl -> %[6]s }
    ch_%[1]s = r_%[1]s.mix(s_%[1]s).first()
`, c.Name, src, field, fused, foldCallArgs(g, p, c.Bindings, enabled, bind), nullBundle(nulls))
}

// nativeDisableGate reports whether a call's disable flag can be read natively
// (no DISABLE task) — when the `disabled` ref is a single top-level field of the
// pipeline args (self.<field>) or an upstream output (CALL.out.<field>). It
// returns the source channel (pa or ch_<call>) and the field name. A nested or
// projected ref keeps the general DISABLE-bind path.
func nativeDisableGate(c ir.Call) (string, string, bool) {
	r := c.Disabled
	if r == nil {
		return "", "", false
	}

	switch r.Kind {
	case ir.RefKindSelf:
		// self.<path>: the flag is a field of the pipeline args (pa) — the whole
		// referenced input (Output == "") or a nested struct path within it. Both
		// are readable from pa's sidecar on the driver (#209).
		return "pa", joinRefPath(r.ID, r.Output), true
	case ir.RefKindCall:
		// CALL.out.<path>: a field (possibly nested, e.g. config.disable_count) of
		// an upstream output, readable from that call's channel. A valid disable
		// ref always resolves to a scalar bool, so the path is pure struct
		// navigation with no map/array projection (Mro2nf walks it directly). An
		// empty Output would be the whole output bundle, not a bool, so it keeps
		// the DISABLE-task path.
		if r.Output != "" {
			return "ch_" + r.ID, r.Output, true
		}
	}

	return "", "", false
}

// joinRefPath renders a ref's dotted read path from its ID and projection Output:
// the ID alone for a whole-value ref, else ID.Output. Used to address a disable
// flag inside a sidecar (self.<id> is a top-level pipeline-args field; a nested
// self.<id>.<output> descends the struct).
func joinRefPath(id, output string) string {
	if output == "" {
		return id
	}

	return id + "." + output
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
// expression (used to substitute a gated channel for a disabled call). It is
// foldCallArgs with no fold context, so ONE loop defines the invocation-arg
// order for every bind-style process. Every such process takes two final
// broadcast inputs: the shared type manifest (types) and its own bindspec
// (spec.json); see assetsDir. The workflow defines `types` in its main block.
func bindCallArgsPa(bindings []ir.Binding, pa, specName string) string {
	return foldCallArgs(genCtx{}, nil, bindings, pa, specName)
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
		if v.Ref != nil && v.Ref.Kind == ir.RefKindCall && !seen[v.Ref.ID] {
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

	// Every stage-phase process is labeled with its Martian adapter language
	// (py/comp/exec), so the DAG, trace, and reports retain which launcher runs
	// each stage — and `withLabel:lang_<x>` config selectors can target them —
	// across every emission mode (default, -native, -native-runner). A fused
	// chain process carries its consumer's label; each folded link still runs
	// through its own language's launcher inside the script.
	fmt.Fprintf(&b, "  label 'lang_%s'\n", s.Lang)

	if val == "" {
		fmt.Fprintf(&b, "  cpus %d\n  %s", cpusOf(s), staticMem(memOf(s)))
	} else {
		fmt.Fprintf(&b, "  %s\n  %s", dynCpus(val, cpusOf(s)), dynMem(val, memOf(s)))
	}

	// `special` is mrp's scheduler-routing key (MRO_JOBRESOURCES); map it through
	// params.job_resources so a grid run resolves it to clusterOptions. A per-task
	// __special (split-returned per chunk / join) wins over a static key, and is
	// routed on the main/join phases even when the stage declares no static key.
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
	} else if val != "" {
		// No static special, but a split's per-chunk / join __special can still
		// appear at runtime on the main and join phases; route it (the key resolves
		// to '' — a no-op — when no __special is returned).
		fmt.Fprintf(&b, "\n  clusterOptions { params.job_resources?.get(%s?.special ?: '') ?: '' }", val)
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
	return fmt.Sprintf("cpus { def t = Math.abs((%[1]s?.threads ?: 0) as double); t > 0 ? Math.max(1, Math.ceil(t) as int) : %[2]d }", val, fallback)
}

// dynMem renders a dynamic `memory` directive: the runtime val's mem_gb when
// positive, else the stage's static `using` default. A negative per-task value is
// the adaptive sentinel and is provisioned at its magnitude (see cpusOf/memOf).
func dynMem(val string, fallback int) string {
	return fmt.Sprintf("memory { def m = Math.abs((%[1]s?.mem_gb ?: 0) as double); m = m > 0 ? m : %[2]d; (m * task.attempt) + ' GB' }", val, fallback)
}

// staticMem renders a static `memory` directive that grows with task.attempt —
// the --auto-adjust-memory analog. Attempt 1 requests the stage's `using` value;
// a retry (an OOM kill is a retryable, non-ASSERT failure) requests a multiple,
// so a stage that died for want of memory gets more on the next attempt instead
// of failing identically. cpus do not escalate (more CPUs do not fix an OOM).
func staticMem(memGB int) string {
	return fmt.Sprintf("memory { %d * task.attempt + ' GB' }", memGB)
}

// cpusOf and memOf use the magnitude of the request: a negative value is
// Martian's adaptive sentinel ("at least |x|"), resolved to |x| (matching mrp's
// cluster path), not floored to the 1-unit minimum.
func cpusOf(s *ir.Stage) int {
	t := math.Abs(s.Resources.Threads)
	if t < 1 {
		return 1
	}

	return int(math.Ceil(t))
}

func memOf(s *ir.Stage) int {
	m := math.Abs(s.Resources.MemGB)
	if m < 1 {
		return 1
	}

	return int(math.Ceil(m))
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	return keys
}
