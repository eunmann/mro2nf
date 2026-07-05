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
		// Groovy interpolates ${projectDir} in the script GString before bash sees
		// the line, so single quotes keep the resulting path literal to bash (a $
		// or backtick in the project path must not re-expand). runnerBase is the
		// project dir on a shared FS, or the baked ctrRunner in a container image
		// (where the project dir is not mounted into an isolated task).
		return fmt.Sprintf(`'python3' '%s/run_stage.py' %s -stagecode '%s' -call '%s' -mro '%s'`,
			g.runnerBase, phase, code, g.entry, g.mroFile)
	}

	var cmd strings.Builder

	fmt.Fprintf(&cmd, "'%s' %s -shell '%s' -stagecode '%s' -lang %s -call '%s' -mro '%s'",
		g.mre, phase, g.shell, code, s.Lang, g.entry, g.mroFile)

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
		fmt.Fprintf(&cmd, " -mrjob '%s'", g.mrjob)
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

// keyedCallAlias is the per-call alias under which a callee's fork-key-threaded
// variant (wf_<callable>_map) is imported into a keyed pipeline.
func keyedCallAlias(pipeline, call string) string {
	return "wfk_" + qualify(pipeline, call)
}

// genKeyedPipeIncludes imports the fork-key-threaded variant of each callee so a
// keyed pipeline can run its body per fork. A fused leaf call or a value-only
// nested-map scatter embeds its stage's main directly (in its _K / _KS process),
// so it needs no wf_<stage>_map import.
func genKeyedPipeIncludes(b *strings.Builder, p *ir.Pipeline, prog *ir.Program, g genCtx) {
	pp := g.plan.pipes[p.Name]

	for _, c := range p.Calls {
		if pp.calls[c.Name].keyedFusedInclude() {
			continue
		}

		_, src := calleeModule(prog, c.Callable)
		fmt.Fprintf(b, "include { wf_%s_map as %s } from '%s'\n", c.Callable, keyedCallAlias(p.Name, c.Name), src)
	}

	b.WriteString("\n")
}

// genKeyedPipeProcesses emits the fork-key-carrying bind processes a keyed
// pipeline uses (one per call, plus the return), switching on each call's
// planned keyedKind.
func genKeyedPipeProcesses(b *strings.Builder, p *ir.Pipeline, prog *ir.Program, g genCtx) {
	pp := g.plan.pipes[p.Name]

	for _, c := range p.Calls {
		cp := pp.calls[c.Name]

		switch cp.keyedKind {
		// A value-only nested map uses the driver element scatter (#99): its
		// fused per-inner-fork process replaces the FORK_K resolve.
		case keyedScatter:
			genKeyedNestedScatterProcess(b, p.Name, c, cp.keyedStage, g)
			genKeyedMergeProcess(b, p.Name, c, calleeOutNames(prog, c.Callable), g)

		case keyedForkBind:
			genKeyedForkBindProcess(b, p.Name, c, g)
			genKeyedMergeProcess(b, p.Name, c, calleeOutNames(prog, c.Callable), g)

			// A natively-gated keyed disable (self.<field>) reads the flag from
			// the per-fork args and needs no DISABLE_K bind.
			if cp.keyedDisableTask {
				genKeyedBindProcess(b, disableName(p.Name, c.Name), disableBindings(c), g, "", "disable")
			}

		// A fuseable leaf call runs bind+main in one keyed process (#99); an
		// unfuseable one keeps its standalone BIND_K feeding the callee's variant.
		case keyedFused:
			genKeyedFusedStageProcess(b, p.Name, c, cp.keyedStage, g)

		case keyedBind:
			genKeyedBindProcess(b, bindName(p.Name, c.Name), c.Bindings, g, g.producerArgs(c.Callable, types.RoleIn), "args")

			// A non-mapped keyed disabled call is gated by genKeyedCallBody, which
			// still uses the keyed DISABLE bind — so keep emitting it here.
			if cp.keyedDisableTask {
				genKeyedBindProcess(b, disableName(p.Name, c.Name), disableBindings(c), g, "", "disable")
			}
		}
	}

	// A pure-forward return needs no keyed return bind — genKeyedPipeline emits
	// the producer's own per-fork bundle (#99).
	if pp.retFwd == "" {
		genKeyedBindProcess(b, bindName(p.Name, "return"), p.Returns, g, g.producerArgs(p.Name, types.RoleOut), `outs__${key}`)
	}
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

// genKeyedFusedStageProcess emits a per-call fork-keyed process that runs
// `mre bind` then the stage's `main` in ONE task — the keyed-layer analog of
// genFusedStageProcess (#16), folding away the standalone BIND_K a keyed leaf
// call would otherwise run per outer fork (#99). Input/output match the
// BIND_K→_MAP pair it replaces: tuple(key, pipeargs, in_refs) + types + spec in,
// tuple(key, outs__<key>) out — so genKeyedCallBody drops it in with no other
// wiring change.
func genKeyedFusedStageProcess(b *strings.Builder, pipeline string, c ir.Call, s *ir.Stage, g genCtx) {
	block, arg := keyedInputs(refCalls(c.Bindings))
	main := g.stageCmd("main", s, vmemFlag(s, "main"))

	fmt.Fprintf(b, `process %[1]s_K {
%[2]s
  input:
%[3]s  output:
    tuple val(key), path("outs__${key}", type: 'dir')
  script:
    """
    '%[4]s' bind -spec 'spec.json' -pipeargs ${pipeargs}%[5]s -o args%[6]s
    %[7]s -args args%[8]s -threads ${task.cpus} -memgb ${task.memory.toGiga()} -work . -o outs__${key}
    """
}

`, fusedName(pipeline, c.Name), stageDirectives(s, ""), block, g.mre, arg,
		g.producerArgs(c.Callable, types.RoleIn), main,
		g.producerArgs(c.Callable, types.RoleMainOut))
}

// keyedInputs renders a keyed process's input block — tuple(key, pipeargs, staged
// upstream bundles) — and the matching `-inputs id=in_id` argument.
func keyedInputs(refs []string) (string, string) {
	// stageAs pins pipeargs off the `args` output name it would otherwise alias
	// when the enclosing pipeline's args are an upstream bind output. See bug 1.
	return keyedInputsHead("tuple val(key), path(pipeargs, stageAs: 'pipeargs')", refs)
}

// keyedInputsHead is keyedInputs with a caller-supplied leading tuple line
// (e.g. the keyed nested scatter's okey/index/element-carrying head), so every
// keyed input block shares ONE per-ref staging loop and fixes land everywhere.
func keyedInputsHead(head string, refs []string) (string, string) {
	var in strings.Builder

	in.WriteString("    " + head)

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

// genKeyedNestedScatterProcess emits the O(1)-per-instance fused inner scatter
// for a value-only nested map (#99): the driver already sliced each outer
// fork's inner collection (Mro2nf.forkElementsKeyed), so an inner fork with
// index > 0 assembles its args from its own base64 element (forkbind
// -elementfile) and runs the stage main — no per-outer-fork FORK_K resolve.
// Index 0 (per outer fork) runs `forkbind -index 0` once to write that outer
// fork's inner forkkeys sidecar for the gather; the fi<0 sentinel validates an
// empty inner collection. The composite key (outer~inner) threads fork identity
// for the regroup; the keys output is keyed by the OUTER key so the per-outer
// merge pairs them. The staged `pipeargs` is that outer fork's args (carried in
// the element tuple), used for the inner broadcast bindings and the index-0
// resolve.
func genKeyedNestedScatterProcess(b *strings.Builder, pipeline string, c ir.Call, s *ir.Stage, g genCtx) {
	block, arg := keyedInputsHead(
		"tuple val(okey), val(key), val(fi), val(element), path(pipeargs, stageAs: 'pipeargs')",
		refCalls(c.Bindings))

	fmt.Fprintf(b, `process %[1]s_KS {
%[2]s
  input:
%[3]s  output:
    tuple val(key), path("outs__${key}", type: 'dir'), emit: outs, optional: true
    tuple val(okey), path('forkkeys.mro2nf.json'), emit: keys, optional: true
%[4]s}

`, forkName(pipeline, c.Name), stageDirectives(s, ""), block, g.scatterScript(c, s, arg))
}

// scatterScript renders the three-branch script block of an element-scatter
// process (#99), shared by the -native plain scatter and the keyed nested
// scatter so a scatter-script fix lands once. fi<0 is the empty-collection
// sentinel (keys-only resolve); fi==0 runs `forkbind -index 0` once, writing
// the forkkeys sidecar alongside its own fork; every other instance assembles
// its args from its own base64 element. The trailing [ -d outs__${key} ] keeps
// the hard per-fork guarantee: an instance that exits 0 without its out bundle
// fails the task instead of silently shortening the merge. arg is the staged-
// ref `-inputs` tail returned for the same process's input block.
func (g genCtx) scatterScript(c ir.Call, s *ir.Stage, arg string) string {
	// The element branch keeps the bare forkbind base: -mapmode is a
	// full-collection flag forkbind rejects alongside -elementfile.
	forkbind := fmt.Sprintf("'%s' forkbind -spec 'spec.json' -pipeargs pipeargs%s", g.mre, arg)
	forkbindAll := fmt.Sprintf("%s -mapmode %s", forkbind, mapModeArg(c))
	main := fmt.Sprintf("%s -args fargs%s -threads ${task.cpus} -memgb ${task.memory.toGiga()} -work . -o outs__${key}",
		g.stageCmd("main", s, vmemFlag(s, "main")),
		g.producerArgs(c.Callable, types.RoleMainOut))

	return fmt.Sprintf(`  script:
    if( fi < 0 )
      """
      %[1]s -keysonly -keysfile forkkeys.mro2nf.json
      """
    else if( fi == 0 )
      """
      %[1]s -index 0 -o fargs -keysfile forkkeys.mro2nf.json
      %[2]s
      [ -d outs__${key} ]
      """
    else
      """
      printf %%s '${element}' | base64 -d > element.json
      %[3]s -elementfile element.json -o fargs
      %[2]s
      [ -d outs__${key} ]
      """
`, forkbindAll, main, forkbind)
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
    '%[3]s' forkbind -spec 'spec.json' -pipeargs ${pipeargs}%[4]s -chunkdir forks -mapmode %[6]s%[5]s
    mv -f forks/forkkeys.json forkkeys.json
    """
}

`, forkName(pipeline, c.Name), block, g.mre, arg, g.producerArgs(c.Callable, types.RoleIn), mapModeArg(c))
}

// genKeyedMergeProcess emits the fork-key-threaded merge for a nested map call:
// per outer fork it gathers that fork's inner results. emptyNull threads here
// for consistency with the plain MERGE (#99), though it cannot fire from valid
// Martian source today: the only keyed split source the plan flags is a
// LITERAL (sub-pipeline self refs are unflagged), and Martian's grammar
// rejects an empty literal split (`split []` is a parse error) — so a flagged
// keyed merge always has at least one fork.
func genKeyedMergeProcess(b *strings.Builder, pipeline string, c ir.Call, calleeOuts string, g genCtx) {
	fmt.Fprintf(b, `process %[1]s_K {
  input:
    tuple val(key), path(souts), path('forkkeys.json')
    path 'types.json'
  output:
    tuple val(key), path('merged', type: 'dir')
  script:
    """
    %[2]s
    """
}

`, mergeName(pipeline, c.Name),
		g.mergeCmd(c.Callable, calleeOuts, "outs__*", "forkkeys.json", "merged",
			g.plan.pipes[pipeline].calls[c.Name].emptyNull))
}

// genKeyedPipeline emits wf_<pipeline>_map: the pipeline body run once per fork,
// with the fork key threaded through every bind and callee. Each channel is
// collapsed to a value list (toList) so it can be re-read by multiple consumers
// without exhausting the fork queue; binds join their inputs by key.
func genKeyedPipeline(b *strings.Builder, p *ir.Pipeline, g genCtx) {
	pp := g.plan.pipes[p.Name]

	var body strings.Builder

	body.WriteString("  main:\n    pa_l = keyed.toList()\n")
	body.WriteString("    types = file(\"${projectDir}/_assets/types.json\")\n")

	for _, c := range p.Calls {
		genKeyedCallBody(&body, p.Name, c, pp.calls[c.Name])
	}

	// A pure-forward return (retFwd) is the producer's own per-fork bundle — no
	// keyed return BIND_K, one fewer task per outer fork (#99). Re-expand its
	// collected list back to per-fork tuple(key, outs__<key>), matching the
	// return bind's emit shape.
	if fwd := pp.retFwd; fwd != "" {
		emit := fmt.Sprintf("ch_%s_l.flatMap { x -> x }", fwd)
		fmt.Fprintf(b, `workflow wf_%s_map {
  take: keyed
%s  emit:
    %s
}

`, p.Name, body.String(), emit)

		return
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
func genKeyedCallBody(body *strings.Builder, pipeline string, c ir.Call, cp callPlan) {
	if c.Mapped {
		genKeyedMappedCallBody(body, pipeline, c, cp)

		return
	}

	// A fuseable leaf call runs bind+main in one keyed process (fusedName_K),
	// taking the same keyed input the standalone BIND_K would and emitting the
	// per-fork outs bundle directly — no separate BIND_K, no _map hop (#99).
	if cp.keyedKind == keyedFused {
		fmt.Fprintf(body, "    ch_%s_l = %s_K(%s).toList()\n",
			c.Name, fusedName(pipeline, c.Name), keyedBindCall(c.Bindings, bindName(pipeline, c.Name)))

		return
	}

	bind := bindName(pipeline, c.Name)
	alias := keyedCallAlias(pipeline, c.Name)
	fmt.Fprintf(body, "    %s_K(%s)\n", bind, keyedBindCall(c.Bindings, bind))

	// The DISABLE_K join must agree with genKeyedPipeProcesses' decision to
	// emit that process, so both read the plan's keyedDisableTask — the same
	// value, never re-derived from c.Disabled here.
	if !cp.keyedDisableTask {
		fmt.Fprintf(body, "    ch_%s_l = %s(%s_K.out).toList()\n", c.Name, alias, bind)

		return
	}

	dis := disableName(pipeline, c.Name)
	nulls := "${projectDir}/nulls/" + qualify(pipeline, c.Name)
	fmt.Fprintf(body, "    %s_K(%s)\n", dis, keyedBindCall(disableBindings(c), dis))
	fmt.Fprintf(body, `    g_%[1]s = %[2]s_K.out.join(%[3]s_K.out).branch { key, args, d ->
        def off = Mro2nf.disabled(d)
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
func genKeyedMappedCallBody(body *strings.Builder, pipeline string, c ir.Call, cp callPlan) {
	fork := forkName(pipeline, c.Name)
	merge := mergeName(pipeline, c.Name)
	alias := keyedCallAlias(pipeline, c.Name)

	// A value-only nested map collapses its per-outer-fork FORK_K into a driver
	// element scatter (#99): the driver slices each outer fork's inner collection
	// once (forkElementsKeyed) and each inner fork assembles its args from its own
	// element — no FORK_K resolve. The MERGE_K gather is unchanged (fed the
	// scatter's composite-keyed outs + per-outer keys).
	if cp.keyedKind == keyedScatter {
		fmt.Fprintf(body, "    ek_%[1]s = pa_l.flatMap { x -> x }.flatMap { ok, pab -> Mro2nf.forkElementsKeyed(ok, pab, '%[2]s', '%[3]s') }\n",
			c.Name, cp.keyedScatterField, mapModeArg(c))
		fmt.Fprintf(body, "    io_%[1]s = %[2]s_KS(ek_%[1]s, types, %[3]s)\n",
			c.Name, fork, specFile(bindName(pipeline, c.Name)))
		// groupTuple's per-group order is completion order; sorting the grouped
		// bundles by name pins MERGE_K's input order — part of its -resume cache
		// key — so a replay stays CACHED regardless of arrival order (the output
		// bytes are already order-safe via the in-task `sort -V`).
		fmt.Fprintf(body, "    mj_%[1]s = io_%[1]s.keys.join(io_%[1]s.outs.map { ck, bdl -> tuple(Mro2nf.outerKey(ck), bdl) }.groupTuple(), remainder: true).map { ok, fk, so -> tuple(ok, (so ?: []).sort { a, b -> a.name <=> b.name }, fk) }\n", c.Name)
		fmt.Fprintf(body, "    %s_K(mj_%s, types)\n", merge, c.Name)
		fmt.Fprintf(body, "    ch_%[1]s_l = %[2]s_K.out.toList()\n", c.Name, merge)

		return
	}

	// A disabled nested map is all-or-nothing per outer fork: the disable flag is
	// resolved per key and gates whether that fork's inner map runs at all. The
	// FORKBIND reuses the call's bind spec (see genKeyedForkBindProcess).
	forkInput := keyedBindCall(c.Bindings, bindName(pipeline, c.Name))
	if c.Disabled != nil {
		forkInput = genKeyedMappedDisableGate(body, pipeline, c, cp)
	}

	fmt.Fprintf(body, "    %s_K(%s)\n", fork, forkInput)
	// Enumerate the per-fork bundle dirs from forknames.json rather than a java.io
	// listFiles() call, which cannot list an object-store (s3://) work dir. Reading
	// the staged names file and constructing each fork path is object-store-safe.
	fmt.Fprintf(body, "    ik_%[1]s = %[2]s_K.out.forks.flatMap { ok, d -> Mro2nf.forkTuples(ok, d) }\n", c.Name, fork)
	fmt.Fprintf(body, "    io_%[1]s = %[2]s(ik_%[1]s)\n", c.Name, alias)
	// Sorted for the same -resume cache-key reason as the scatter path above.
	fmt.Fprintf(body, "    mj_%[1]s = %[2]s_K.out.keys.join(io_%[1]s.map { ck, bdl -> tuple(Mro2nf.outerKey(ck), bdl) }.groupTuple(), remainder: true).map { ok, fk, so -> tuple(ok, (so ?: []).sort { a, b -> a.name <=> b.name }, fk) }\n", c.Name, fork)
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
func genKeyedMappedDisableGate(body *strings.Builder, pipeline string, c ir.Call, cp callPlan) string {
	nulls := "${projectDir}/nulls/" + qualify(pipeline, c.Name)

	// Native gating (#59, Lever 2) for a self.<field> flag: read it from the
	// per-fork pipeline-args bundle (row[1]) — no keyed DISABLE task, no join.
	// The DECISION is the plan's keyedDisableTask (set from keyedNativeDisable,
	// so it cannot drift from genKeyedPipeProcesses' DISABLE_K emission);
	// nativeDisableGate only extracts the field name here. (An upstream-ref
	// keyed disable keeps the DISABLE_K bind: its position in the keyed row
	// depends on the bind's ref order.)
	if !cp.keyedDisableTask {
		_, field, _ := nativeDisableGate(c)
		fmt.Fprintf(body, `    gk_%[1]s = %[2]s.branch { row ->
        def off = Mro2nf.disabledDir(row[1], '%[3]s')
        run: !off
        skip: off
    }
    sk_%[1]s = gk_%[1]s.skip.map { row -> tuple(row[0], file("%[4]s")) }
`, c.Name, keyedBindInput(c.Bindings), field, nulls)

		return fmt.Sprintf("gk_%s.run, types, %s", c.Name, specFile(bindName(pipeline, c.Name)))
	}

	dis := disableName(pipeline, c.Name)

	fmt.Fprintf(body, "    %s_K(%s)\n", dis, keyedBindCall(disableBindings(c), dis))
	fmt.Fprintf(body, `    gk_%[1]s = %[2]s.join(%[3]s_K.out).branch { row ->
        def off = Mro2nf.disabled(row[-1])
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

// keyedReachable returns the set of stage AND pipeline names that can run under a
// map call — directly targeted by a `map call`, or reached through a (possibly
// nested) sub-pipeline that is map-called. Only these callables need a fork-keyed
// variant emitted (a stage's _MAP/_SPLIT_K processes, a pipeline's wf_<p>_map
// plus its keyed includes/processes); every other callable's keyed layer is
// never invoked. The analysis unions over all call paths, so a callable reached
// both keyed and plain is still marked keyed. A plain-context map call planned
// as a native scatter (#76) runs the stage inline in its own fused process, so
// it does NOT fan out to the callee's keyed variant — read from the already-
// decided plan kind, never re-derived, so this site cannot disagree with
// planCall (#77). The same call inside a keyed pipeline still forks through
// FORK_K, which does need the keyed variant.
func keyedReachable(prog *ir.Program, pl emitPlan) map[string]bool {
	needed := map[string]bool{}
	seen := map[[2]string]bool{} // (callable, keyed-context) already walked

	var walk func(callable string, keyed bool)
	walk = func(callable string, keyed bool) {
		if _, ok := prog.Stages[callable]; ok {
			if keyed {
				needed[callable] = true
			}

			return
		}

		p, ok := prog.Pipelines[callable]
		if !ok {
			return
		}

		if keyed {
			needed[callable] = true
		}

		mark := [2]string{callable, boolKey(keyed)}
		if seen[mark] {
			return
		}

		seen[mark] = true

		for _, c := range p.Calls {
			// A fuseable keyed leaf call embeds its stage's main in a per-call
			// fused process (genKeyedFusedStageProcess), so it needs no
			// wf_<stage>_map variant — don't mark the callee keyed-reachable
			// (matches genKeyedPipeIncludes skipping its import).
			// pl.pipes is complete before keyedReachable runs (buildPlan fills
			// every pipeline first); a map miss here would read the zero
			// callPlan (keyedBind) and silently mis-mark reachability, so that
			// ordering is load-bearing.
			if keyed && pl.pipes[callable].calls[c.Name].keyedKind == keyedFused {
				continue
			}

			mapped := c.Mapped
			if mapped && !keyed && pl.pipes[callable].calls[c.Name].kind == kindNativeScatter {
				mapped = false
			}

			walk(c.Callable, keyed || mapped)
		}
	}

	// Walk EVERY declared pipeline as a root, not just the ones reachable from the
	// entry: writeModules emits a module for every pipeline in the program
	// (including a declared-but-uncalled one from an @included library), and a
	// pipeline module's plain includes reference wf_<callee>_map for each of its
	// `map call`s. So those keyed variants must be emitted even for a pipeline the
	// entry never reaches, or the include dangles. The `seen` guard dedups the
	// overlap with the entry-reachable walk.
	for name := range prog.Pipelines {
		walk(name, false)
	}

	return needed
}

func boolKey(b bool) string {
	if b {
		return "k"
	}

	return "p"
}

// genKeyedSplitProcesses emits fork-key-carrying variants of the split/main/join
// processes: every channel item is tuple(key, ...), so chunks and joins stay
// partitioned by fork. Outputs are named by key so the merge orders them.
func genKeyedSplitProcesses(b *strings.Builder, s *ir.Stage, g genCtx, base string) {
	splitCmd := g.stageCmd("split", s, vmemFlag(s, "split"))
	joinCmd := g.stageCmd("join", s, vmemFlag(s, "join"))

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
    %[3]s -args ${args} -work . -o chunks.json -joinres joinres.json -chunkdir . -threads ${task.cpus} -memgb ${task.memory.toGiga()}%[4]s
    """
}

process %[1]s_MAIN_K {
%[9]s
  input:
    tuple val(key), val(res), path(chunk), path(args)
    path 'types.json'
  output:
    tuple val(key), path("out_${chunk.baseName}", type: 'dir')
  script:
    """
    %[5]s -args ${args} -chunk ${chunk}%[7]s -threads ${task.cpus} -memgb ${task.memory.toGiga()} -work . -o out_${chunk.baseName}
    """
}

process %[1]s_JOIN_K {
%[10]s
  input:
    tuple val(key), val(join), path(souts), path(args), path(defs)
    path 'types.json'
  output:
    tuple val(key), path("outs__${key}", type: 'dir')
  script:
    """
    %[6]s -args ${args} -chunkdefs ${defs} -chunkouts "\$(ls -1d out_* 2>/dev/null | sort -V | paste -sd, -)"%[8]s -threads ${task.cpus} -memgb ${task.memory.toGiga()} -work . -o outs__${key}
    """
}

`, s.Name, stageDirectives(s, ""), splitCmd, g.producerArgs(s.Name, types.RoleChunkIn),
		base, joinCmd, g.producerArgs(s.Name, types.RoleMainOut),
		g.producerArgs(s.Name, types.RoleOut),
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
    chunks = %[1]s_SPLIT_K.out.chunks.flatMap { key, cs -> Mro2nf.keyedChunks(key, cs) }
    %[1]s_MAIN_K(chunks.combine(ch.mn, by: 0), types)
    joinres = %[1]s_SPLIT_K.out.joinres.map { key, f -> tuple(key, Mro2nf.parseJson(f)) }
    // Drive the join from the full fork set (ch.jn) with a remainder join on the
    // chunk outputs, so a fork whose split produced zero chunks (no groupTuple
    // group) still runs JOIN_K — with an empty chunk-outs list — instead of
    // being dropped. defs/joinres are inner-joined (always emitted per fork).
    // The grouped chunk outs are sorted by name: groupTuple order is completion
    // order, and JOIN_K's input order is part of its -resume cache key.
    joined = ch.jn.join(%[1]s_SPLIT_K.out.defs).join(joinres).join(%[1]s_MAIN_K.out.groupTuple(), remainder: true).map { t -> tuple(t[0], t[3], (t[4] ?: []).sort { a, b -> a.name <=> b.name }, t[1], t[2]) }
    %[1]s_JOIN_K(joined, types)
  emit:
    %[1]s_JOIN_K.out
}

`, s.Name)
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

// genKeyedSingleStage emits a fork-key-threaded variant of a non-split stage:
// the process carries tuple(key, args) and names its output bundle by key so a
// map call (or an enclosing keyed pipeline) can run one instance per fork and
// gather per fork.
func genKeyedSingleStage(b *strings.Builder, s *ir.Stage, base string, g genCtx) {
	fmt.Fprintf(b, `process %[1]s_MAP {
%[2]s
  input:
    tuple val(key), path(args)
    path 'types.json'
  output:
    tuple val(key), path("outs__${key}", type: 'dir')
  script:
    """
    %[3]s -args ${args}%[4]s -threads ${task.cpus} -memgb ${task.memory.toGiga()} -work . -o outs__${key}
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

`, s.Name, stageDirectives(s, ""), base, g.producerArgs(s.Name, types.RoleMainOut))
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
    path defs
    path souts
    path 'types.json'
  output:
    %[13]s
  script:
    """
    %[5]s -args args -chunkdefs ${defs} -chunkouts "\$(ls -1d out_* 2>/dev/null | sort -V | paste -sd, -)"%[8]s -threads ${task.cpus} -memgb ${task.memory.toGiga()} -work . -o outs
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
    %[1]s_JOIN(join, a, %[1]s_SPLIT.out.defs, %[1]s_MAIN.out.toSortedList { x, y -> x.name <=> y.name }, types)
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

// genNativeScatterElementProcess emits the O(1)-per-instance fused
// forkbind+main process a -native VALUE-only map call scatters over (#76/#99):
// no FORK task, and no per-instance collection re-parse. The driver sliced the
// collection once, so a fork with index > 0 assembles its args from its own
// base64 JSON element (`forkbind -elementfile`). Index 0 runs `forkbind -index
// 0` once to write the forkkeys sidecar for the gather (parsing the collection
// once — ONE instance, not N; a single deterministic producer, so -resume
// rehashes the same path). The index -1 sentinel (empty fork collection) runs
// `forkbind -keysonly`: every binding is resolved and validated exactly as the
// always-running FORK task did — a wrong-kind or mis-zipped source still fails
// loudly — and only the keys sidecar is produced, so both outputs are optional.
// Out bundles are named outs__<key> so the gather's `sort -V` staging orders
// forks exactly as the keyed path does; the sidecar's task-side name is
// forkkeys.mro2nf.json because the stage main shares this cwd (-work .), so a
// stage scratch file named forkkeys.json must not satisfy the optional keys
// output (consumers rename it on staging anyway). The trailing [ -d
// outs__<key> ] keeps the hard per-fork guarantee: an instance that exits 0
// without its out bundle fails the task instead of silently shortening an
// array-fork merge. The pipeargs bundle is a broadcast input (staged once per
// instance for the broadcast bindings) in a value context; in a queue-pipeargs
// pipeline (scatterQueuedPa) each element tuple carries it instead — a ≤1-item
// queue cannot broadcast to N instances. The script is identical either way:
// the staged dir is `pipeargs` in both layouts.
func genNativeScatterElementProcess(b *strings.Builder, pipeline string, c ir.Call, cp callPlan, g genCtx) {
	s := cp.stage

	head := "tuple val(key), val(fi), val(element)\n    " + bundleInput("pipeargs")
	if cp.scatterQueuedPa {
		head = "tuple val(key), val(fi), val(element), " + bundleInputElems("pipeargs")
	}

	block, arg := bindInputsHead(head, refCalls(c.Bindings))

	fmt.Fprintf(b, `process %[1]s {
%[2]s
  input:
%[3]s  output:
    path "outs__${key}", type: 'dir', emit: outs, optional: true
    path 'forkkeys.mro2nf.json', emit: keys, optional: true
%[4]s}

`, fusedName(pipeline, c.Name), stageDirectives(s, ""), block, g.scatterScript(c, s, arg))
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
    %[5]s(join, a, %[3]s.out.defs, %[4]s.out.toSortedList { x, y -> x.name <=> y.name }, types)
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

// genNativeScatterWiring wires a -native map call's O(1) element scatter (#99):
// the driver slices the split collection ONCE into per-fork elements and fans
// out one fused instance per fork — no FORK task, no per-instance collection
// re-parse; an empty collection scatters the keys-only sentinel instead. The
// keys channel has exactly one producer (the fi<=0 instance), so .first() is
// deterministic — and when the enclosing pipeline is skipped (no pipeargs
// item, so no instances at all) the keys channel stays empty and the gather
// stays dormant, exactly like FORK.out.keys on the FORK path.
func genNativeScatterWiring(b *strings.Builder, pipeline string, c ir.Call, cp callPlan) {
	fused := fusedName(pipeline, c.Name)

	switch {
	case cp.scatterQueuedPa:
		// Queue-pipeargs pipeline (a disabled sub-pipeline callee): the ≤1-item
		// pa queue cannot broadcast into N instances, so each element tuple
		// carries the pipeargs bundle alongside its element. Upstream refs are
		// barred here (nativeScatterable), so pipeargs is the only source.
		fmt.Fprintf(b, "    scat_%s = pa.flatMap { data, leaves -> Mro2nf.forkElementsPa(data, leaves, '%s', '%s') }\n",
			c.Name, cp.scatterField, mapModeArg(c))
		fmt.Fprintf(b, "    %s(%s)\n", fused,
			bindCallArgsPa(c.Bindings, "scat_"+c.Name, bindName(pipeline, c.Name)))

	case cp.scatterCall != "":
		// Upstream source: slice the producer's value-channel bundle — safe to
		// read directly (value channel, never fused away, never rewritten to
		// *_souts); the producer is ALSO staged into each instance as the
		// in_<id> broadcast the -index-0 keys resolve and broadcast bindings
		// read. pipeargs broadcasts separately (`pa` in elementCallArgs).
		fmt.Fprintf(b, "    scat_%[1]s = ch_%[2]s.flatMap { data, leaves -> Mro2nf.forkElements(data, '%[3]s', '%[4]s') }\n",
			c.Name, cp.scatterCall, cp.scatterField, mapModeArg(c))
		fmt.Fprintf(b, "    %s(%s)\n", fused, elementCallArgs(c, "scat_"+c.Name, bindName(pipeline, c.Name)))

	default:
		// Self source in a value context: slice pipeargs' collection; pipeargs
		// broadcasts separately so each instance stages it once for the
		// broadcast bindings.
		fmt.Fprintf(b, "    scat_%s = pa.flatMap { data, leaves -> Mro2nf.forkElements(data, '%s', '%s') }\n",
			c.Name, cp.scatterField, mapModeArg(c))
		fmt.Fprintf(b, "    %s(%s)\n", fused, elementCallArgs(c, "scat_"+c.Name, bindName(pipeline, c.Name)))
	}

	// The gathered outs are sorted by bundle name driver-side so the consumer's
	// input-file ORDER is deterministic across runs — collect() order is
	// completion order, which would invalidate the consumer's -resume cache key
	// on every replay. toSortedList also emits [] for an empty channel (the
	// ifEmpty it replaces), so it is NOT dormancy-safe on its own: when the
	// enclosing pipeline is skipped the souts channel still emits []. Dormancy
	// rests on the keys channel — .first() of the single fi<=0 writer never
	// binds when no instance ran — so every consumer must take souts AND keys
	// together (foldBindInputs guarantees the pairing).
	souts := fused + ".out.outs.toSortedList { a, b -> a.name <=> b.name }"

	// A folded merge runs inside the sole consumer's task (#76): expose the
	// gathered outs and the keys value channel for it instead of a MERGE task.
	if cp.foldMerge {
		fmt.Fprintf(b, "    %s = %s\n", soutsChan("ch_", c.Name), souts)
		fmt.Fprintf(b, "    %s = %s.out.keys.first()\n", keysChan("ch_", c.Name), fused)

		return
	}

	merge := mergeName(pipeline, c.Name)
	fmt.Fprintf(b, "    %s(%s, %s.out.keys.first(), types)\n", merge, souts, fused)
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
		// self.<field>: the whole referenced input is the flag (no sub-path).
		if r.Output == "" {
			return "pa", r.ID, true
		}
	case ir.RefKindCall:
		// CALL.out.<field>: a single (non-nested) output field.
		if r.Output != "" && !strings.Contains(r.Output, ".") {
			return "ch_" + r.ID, r.Output, true
		}
	}

	return "", "", false
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

// elementCallArgs renders the invocation for the O(1) element scatter process
// (#99): the element tuple channel first, then the standard bind-style args with
// pipeargs (`pa`) as the SEPARATE broadcast input — so the same one loop
// (bindCallArgsPa/foldCallArgs) defines the ref/types/spec order the process's
// input block reads, and the two can't drift.
func elementCallArgs(c ir.Call, scatChan, specName string) string {
	return scatChan + ", " + bindCallArgsPa(c.Bindings, "pa", specName)
}

// genPublish emits the terminal publish as two processes that avoid a single-node
// funnel (#12): LAYOUT stages ONLY the final sidecar (data.json) — no file leaves
// — to compute the outs/ layout (leaf basename -> outs/ rel path) and the
// pipeline_outs.json value tree; PUBLISH_LEAF then stages each leaf individually
// and publishes it into outs/<rel>, so the result set is published in parallel
// across tasks rather than round-tripped through one node. The physical outs/
// tree (which downstream pipelines consume) is unchanged.
func genPublish(b *strings.Builder, prog *ir.Program, g genCtx) {
	if p := nativeReturnBind(prog, g); p != nil {
		genNativeLayout(b, prog, p, g)
	} else {
		genDefaultLayout(b, prog.Entry.Callable, g)
	}

	fmt.Fprint(b, `
process PUBLISH_LEAF {
  publishDir params.outdir ?: '.', mode: 'copy', enabled: params.outdir != null, saveAs: { outsPath }
  input:
    tuple val(outsPath), path(leaf)
  output:
    path leaf
  script:
    """
    true
    """
}

`)
}

// genDefaultLayout emits the bundle-mode LAYOUT: it reads the pipeline's return
// bundle sidecar (produced by a separate return BIND) and computes the outs/
// layout + manifest.
func genDefaultLayout(b *strings.Builder, entry string, g genCtx) {
	fmt.Fprintf(b, `process LAYOUT {
  publishDir params.outdir ?: '.', mode: 'copy', enabled: params.outdir != null, pattern: '{pipeline_outs.json,manifest.json.gz}'
  input:
    path 'data.json'
    path 'types.json'
  output:
    path 'layout.json', emit: layout
    path 'pipeline_outs.json'
    path 'manifest.json.gz'
  script:
    """
    "%[1]s" publish-layout -sidecar data.json -dir .%[2]s
    """
}
`, g.mre, g.producerArgs(entry, types.RoleOut))
}

// nativeReturnBind returns the entry pipeline whose transform return LAYOUT binds
// inline under -native (folding the return BIND into the publish task), or nil
// when -native is off, the entry is a stage, or the return is a pure forward
// (#14, no BIND to fold).
func nativeReturnBind(prog *ir.Program, g genCtx) *ir.Pipeline {
	if !g.features.native {
		return nil
	}

	p, ok := prog.Pipelines[prog.Entry.Callable]
	if !ok || g.plan.pipes[p.Name].retFwd != "" {
		return nil
	}

	return p
}

// genNativeLayout emits a LAYOUT that runs the pipeline's return BIND inline
// before publish-layout (#76): no standalone BIND_<entry>__return node. It takes
// the return bind's inputs (pipeargs + the returned calls), binds them into the
// return bundle, moves it to the task root so publish-layout runs the identical
// command the default LAYOUT does, and exposes the bundle leaves for PUBLISH_LEAF.
func genNativeLayout(b *strings.Builder, prog *ir.Program, p *ir.Pipeline, g genCtx) {
	inBlock, arg, pre := foldBindInputs(g, prog, p, bundleInput("pipeargs"), refCalls(p.Returns))
	flags := g.producerArgs(p.Name, types.RoleOut)

	fmt.Fprintf(b, `process LAYOUT {
  publishDir params.outdir ?: '.', mode: 'copy', enabled: params.outdir != null, pattern: '{pipeline_outs.json,manifest.json.gz}'
  input:
%[1]s  output:
    path 'layout.json', emit: layout
    path 'pipeline_outs.json'
    path 'manifest.json.gz'
    path 'f/*', emit: leaves, arity: '0..*', optional: true
  script:
    """
%[5]s    '%[2]s' bind -spec 'spec.json' -pipeargs pipeargs%[3]s -o args%[4]s
    mv args/data.json data.json
    if [ -d args/f ]; then mv args/f f; fi
    '%[2]s' publish-layout -sidecar data.json -dir .%[4]s
    """
}
`, inBlock, g.mre, arg, flags, pre)
}

// genNativeEntry emits the native workflow (#76 M1): the entry args are baked
// into entry_resolved/ at transpile time (see bakeEntryArgs), so instead of a
// BUILD_ENTRY_ARGS task the workflow stages that bundle as a value channel and
// runs the pipeline directly. The publish wiring is identical to the default.
func genNativeEntry(b *strings.Builder, prog *ir.Program, entryWorkflow string, g genCtx) {
	// -native bakes entry args at transpile time, so a launch-time override
	// (-params-file / --<input>) would be SILENTLY ignored — fail loudly
	// instead: an ignored override is a silent output divergence (e.g. an
	// emptyNull fork would yield null where the user expected their forks).
	fmt.Fprintf(b, `
workflow {
  %[2]s.each { n ->
    if( params.containsKey(n) )
      error "entry arg '${n}' was baked at transpile time (-native); re-transpile to change it"
  }
  types = file("${projectDir}/_assets/types.json")
  pipeargs = Channel.value(tuple(file("${projectDir}/entry_resolved/data.json"), file("${projectDir}/entry_resolved/f/*", type: 'any')))
  %[1]s(pipeargs)
`, entryWorkflow, groovyStringList(entryInNames(prog)))

	if p := nativeReturnBind(prog, g); p != nil {
		genNativePublishWiring(b, p, entryWorkflow, g)
	} else {
		genPublishWiring(b, entryWorkflow)
	}

	b.WriteString("}\n")
}

// entryInNames lists the entry callable's input parameter names (pipeline or
// stage entry alike) — the params a -native project has baked and must reject
// at launch.
func entryInNames(prog *ir.Program) []string {
	if p, ok := prog.Pipelines[prog.Entry.Callable]; ok {
		return names(p.In)
	}

	if s, ok := prog.Stages[prog.Entry.Callable]; ok {
		return names(s.In)
	}

	return nil
}

// groovyStringList renders names as a Groovy string-list literal.
func groovyStringList(items []string) string {
	quoted := make([]string, len(items))
	for i, s := range items {
		quoted[i] = "'" + s + "'"
	}

	return "[" + strings.Join(quoted, ", ") + "]"
}

// genNativePublishWiring wires the native LAYOUT (which folds in the return bind):
// it feeds LAYOUT the entry's raw return inputs (pargs + each returned call) and
// takes the published leaves from LAYOUT's own output for PUBLISH_LEAF.
func genNativePublishWiring(b *strings.Builder, p *ir.Pipeline, entryWorkflow string, g genCtx) {
	var refArgs strings.Builder
	for _, id := range refCalls(p.Returns) {
		if _, ok := mergeFoldProducer(g, p, id); ok {
			fmt.Fprintf(&refArgs, ", %[1]s.out.%[2]s, %[1]s.out.%[3]s",
				entryWorkflow, soutsChan("ref_", id), keysChan("ref_", id))

			continue
		}

		fmt.Fprintf(&refArgs, ", %s.out.ref_%s", entryWorkflow, id)
	}

	fmt.Fprintf(b, `  LAYOUT(%[1]s.out.pargs%[2]s, types, %[3]s)
  lmap = LAYOUT.out.layout.map { f -> Mro2nf.parseJson(f) }
  leaves = LAYOUT.out.leaves.flatMap { l -> (l instanceof List ? l : [l]).collect { leaf -> tuple(leaf.name, leaf) } }
  PUBLISH_LEAF(leaves.combine(lmap).flatMap { base, leaf, m -> (m[base] ?: []).collect { rel -> tuple(rel, leaf) } })
`, entryWorkflow, refArgs.String(), specFile(bindName(p.Name, "return")))
}

// entryWiring is the per-input wiring genEntry assembles into the entry
// template: the params.<name> declarations, the file-staging path inputs and
// head-node flatten channels, the values-map pairs, the -fileflat map entries,
// and the BUILD_ENTRY_ARGS call arguments.
type entryWiring struct {
	decls, fileInputs, fileChans string
	pairs, flatPairs, callArgs   []string
}

// entryParamWiring accumulates each entry input's wiring: every input gets a
// nullable params.<name> declaration and a values-map pair; a file-bearing
// input is additionally routed through Nextflow staging — a `path` input, a
// head-node flatten channel (falling back to the empty sentinel when unset),
// and a -fileflat map entry so entryargs can pop the staged paths back in.
func entryParamWiring(prog *ir.Program) entryWiring {
	ins := entryInParams(prog)

	var decls, fileInputs, fileChans strings.Builder

	w := entryWiring{
		pairs:    make([]string, 0, len(ins)),
		callArgs: []string{`file("${projectDir}/entry_args")`, "values", "types"},
	}
	sentinel := fmt.Sprintf(`file("${projectDir}/%s/%s")`, assetsDir, entrySentinel)

	for _, p := range ins {
		fmt.Fprintf(&decls, "params.%s = null\n", p.Name)
		w.pairs = append(w.pairs, fmt.Sprintf("%[1]s: params.%[1]s", p.Name))

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
		// The staged paths (one per file leaf, canonical order) reach entryargs
		// as one JSON object written via heredoc — JSON escaping keeps legal
		// filenames containing , ; = intact, which a flat separator encoding
		// cannot. The list-coercion matters: a single staged file is a lone
		// Groovy Path, and toJson would serialize a bare Path as its segment
		// list; wrapping it in a list and spreading toString() yields one path
		// string per leaf. A multi-leaf input is already a List, left as-is.
		w.flatPairs = append(w.flatPairs, fmt.Sprintf("%[1]s: (%[2]s instanceof List ? %[2]s : [%[2]s])*.toString()", p.Name, in))
		w.callArgs = append(w.callArgs, "flat_"+p.Name)
		// Flatten the override's file leaves to a list of staged files on the head node;
		// an unset input (or one with no file leaves) falls back to the empty sentinel so
		// the process still runs (entryargs ignores the sentinel when the input is unset).
		fmt.Fprintf(&fileChans, "  flat_%[1]s = (params.%[1]s != null ? (%[2]s) : []) ?: [%[3]s]\n",
			p.Name, fileFlattenExpr("params."+p.Name, p, prog.Structs), sentinel)
	}

	w.decls, w.fileInputs, w.fileChans = decls.String(), fileInputs.String(), fileChans.String()

	return w
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
	if g.features.native {
		genNativeEntry(b, prog, entryWorkflow, g)

		return
	}

	w := entryParamWiring(prog)

	// `[:]` is Groovy's empty map (a bare `[]` would be a list, which the overrides
	// JSON object must not be); a non-empty map lists each input's param.
	valuesMap := "[:]"
	if len(w.pairs) > 0 {
		valuesMap = "[" + strings.Join(w.pairs, ", ") + "]"
	}

	// A second quoted heredoc writes the staged-path map as JSON; the flag
	// passes only the filename, so no path ever crosses a shell-quoting seam.
	flatFlag, flatHeredoc := "", ""
	if len(w.flatPairs) > 0 {
		flatFlag = " -fileflat fileflat.json"
		flatHeredoc = fmt.Sprintf(`    cat > fileflat.json <<'MART_EOF'
${groovy.json.JsonOutput.toJson([%s])}
MART_EOF
`, strings.Join(w.flatPairs, ", "))
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
    %[10]s
  script:
    """
    cat > values.json <<'MART_EOF'
${values}
MART_EOF
%[11]s    '%[2]s' entryargs -base entry_args -values values.json -o entry_resolved -types 'types.json' -callable '%[3]s' -role in%[7]s
    """
}

workflow {
  types = file("${projectDir}/_assets/types.json")
  values = groovy.json.JsonOutput.toJson(%[4]s)
%[8]s  BUILD_ENTRY_ARGS(%[9]s)
  pipeargs = BUILD_ENTRY_ARGS.out.first()
  %[5]s(pipeargs)
`, w.decls, g.mre, prog.Entry.Callable, valuesMap, entryWorkflow,
		w.fileInputs, flatFlag, w.fileChans, strings.Join(w.callArgs, ", "),
		bundleOutput("entry_resolved"), flatHeredoc)

	genPublishWiring(b, entryWorkflow)
	b.WriteString("}\n")
}

// genPublishWiring emits the shared publish tail (#12): LAYOUT reads only the
// sidecar to compute the outs/ layout; PUBLISH_LEAF publishes each leaf in
// parallel. multiMap splits the final output tuple so both consume it safely.
// Used by both the default (BUILD_ENTRY_ARGS) and native entry workflows.
func genPublishWiring(b *strings.Builder, entryWorkflow string) {
	fmt.Fprintf(b, `  // Publish without a single-node funnel (#12): LAYOUT reads only the sidecar to
  // compute the outs/ layout; PUBLISH_LEAF publishes each leaf into place in
  // parallel. multiMap splits the final output tuple so both consume it safely.
  ep = %[1]s.out.multiMap { s, l -> side: s; leaves: l }
  LAYOUT(ep.side, types)
  lmap = LAYOUT.out.layout.map { f -> Mro2nf.parseJson(f) }
  leaves = ep.leaves.flatMap { l -> (l instanceof List ? l : [l]).collect { leaf -> tuple(leaf.name, leaf) } }
  PUBLISH_LEAF(leaves.combine(lmap).flatMap { base, leaf, m -> (m[base] ?: []).collect { rel -> tuple(rel, leaf) } })
`, entryWorkflow)
}

// fileFlattenExpr renders a Groovy expression that flattens the file leaves of a
// runtime value (expr, of param p's type) into a list of file() objects, in the
// canonical walk order types.Table uses — arrays in index order, maps by sorted
// key, struct fields in declaration order — so mre entryargs can pop the staged
// paths back into the value in the same order. Non-file scalars contribute [].
func fileFlattenExpr(expr string, p ir.Param, structs map[string]*ir.StructType) string {
	return fileFlattenExprDepth(expr, p, structs, 0)
}

// fileFlattenExprDepth threads a closure-variable depth so nested array/map
// closures bind distinct names (__e0, __e1, ...). Reusing one name across
// nesting depths is a Groovy compile error ("variable already declared").
// Struct fields recurse at the *same* depth: they substitute (expr)?.field
// without opening a new closure, so siblings never collide.
func fileFlattenExprDepth(expr string, p ir.Param, structs map[string]*ir.StructType, depth int) string {
	switch {
	case p.ArrayDim > 0:
		elem := p
		elem.ArrayDim--
		v := fmt.Sprintf("__e%d", depth)

		return fmt.Sprintf("(%s ?: []).collect { %s -> %s }.flatten()", expr, v, fileFlattenExprDepth(v, elem, structs, depth+1))
	case p.MapDim > 0:
		// A typed map is one map level; Martian folds inner array dims into MapDim
		// (MapDim = 1 + innerArrayDim). Descend one level, then treat the value as
		// carrying mapDim-1 array dims — not another typed map.
		val := p
		val.ArrayDim += p.MapDim - 1
		val.MapDim = 0
		v := fmt.Sprintf("__e%d", depth)

		return fmt.Sprintf("(%s ?: [:]).sort { it.key }.collect { %s -> %s }.flatten()", expr, v, fileFlattenExprDepth(v+".value", val, structs, depth+1))
	}

	if st, ok := structs[p.BaseType]; ok {
		parts := make([]string, 0, len(st.Fields))
		for _, f := range st.Fields {
			parts = append(parts, fileFlattenExprDepth(fmt.Sprintf("(%s)?.%s", expr, f.Name), f, structs, depth))
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
