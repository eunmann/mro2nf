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

// refKindCall is the ir.Ref.Kind for a reference to another call's output;
// refKindSelf is a reference to one of the pipeline's own inputs.
const (
	refKindCall = "call"
	refKindSelf = "self"
)

// genCtx carries the resolved paths and names needed to render Nextflow code.
type genCtx struct {
	entry   string
	mroFile string
	mre     string
	shell   string
	mrjob   string
	monitor bool
	// fuseChains opts into linear-chain stage fusion (#59 Lever 4): a
	// single-consumer, equal-resource source stage folds into its consumer's task.
	fuseChains bool
	code       map[string]string // stage name -> stage code path
	// keyed is the set of stage names reachable under a map call, which are the
	// only stages whose fork-keyed variants (_MAP / _SPLIT_K etc.) can be invoked.
	// A stage not in this set gets no keyed variant emitted (#59).
	keyed map[string]bool
}

// stageCmd renders an mre invocation for a stage phase, single-quoting every
// path so spaces and shell metacharacters in paths are safe.
func (g genCtx) stageCmd(phase, code string, lang ir.Lang, vmemExpr string) string {
	cmd := fmt.Sprintf("'%s' %s -shell '%s' -stagecode '%s' -lang %s -call '%s' -mro '%s'",
		g.mre, phase, g.shell, code, lang, g.entry, g.mroFile)

	if g.mrjob != "" {
		cmd += fmt.Sprintf(" -mrjob '%s'", g.mrjob)
	}

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

	genPipeIncludes(&b, p, prog)
	genPipeProcesses(&b, p, prog, g)
	genPipelineWorkflow(&b, p, prog, g)

	// The keyed layer (wf_<p>_map + its keyed includes/processes) is only invoked
	// when this pipeline runs under a map call; otherwise it is dead code (#59).
	if g.keyed[p.Name] {
		genKeyedPipeIncludes(&b, p, prog)
		genKeyedPipeProcesses(&b, p, prog, g)
		genKeyedPipeline(&b, p)
	}

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
		// A fused non-split call is a self-contained per-call process — no import.
		if _, ok := fuseableStageCall(c, p, prog); ok {
			continue
		}

		// A fused split call imports the stage's MAIN/JOIN phase processes, aliased
		// per call (DSL2 requires an alias since wf_<stage> also invokes them).
		if s, ok := fuseableSplitCall(c, p, prog); ok {
			fmt.Fprintf(b, "include { %[1]s_MAIN as %[2]s; %[1]s_JOIN as %[3]s } from './stage_%[1]s.nf'\n",
				s.Name, fusedMainAlias(p.Name, c.Name), fusedJoinAlias(p.Name, c.Name))

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

			// A natively-gated keyed disable (self.<field>) reads the flag from the
			// per-fork args and needs no DISABLE_K bind.
			if c.Disabled != nil && !keyedNativeDisable(c) {
				genKeyedBindProcess(b, disableName(p.Name, c.Name), disableBindings(c), g, "", "disable")
			}

			continue
		}

		genKeyedBindProcess(b, bindName(p.Name, c.Name), c.Bindings, g, g.producerArgs(c.Callable, types.RoleIn), "args")

		// A non-mapped keyed disabled call is gated by genKeyedCallBody, which still
		// uses the keyed DISABLE bind — so keep emitting it here.
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

	// stageAs pins pipeargs off the `args` output name it would otherwise alias
	// when the enclosing pipeline's args are an upstream bind output. See bug 1.
	in.WriteString("    tuple val(key), path(pipeargs, stageAs: 'pipeargs')")

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
    '%[3]s' forkbind -spec 'spec.json' -pipeargs ${pipeargs}%[4]s -chunkdir forks -mapmode %[6]s%[5]s
    mv -f forks/forkkeys.json forkkeys.json
    """
}

`, forkName(pipeline, c.Name), block, g.mre, arg, g.producerArgs(c.Callable, types.RoleIn), mapModeArg(c))
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
	fmt.Fprintf(body, "    ik_%[1]s = %[2]s_K.out.forks.flatMap { ok, d -> Mro2nf.forkTuples(ok, d) }\n", c.Name, fork)
	fmt.Fprintf(body, "    io_%[1]s = %[2]s(ik_%[1]s)\n", c.Name, alias)
	fmt.Fprintf(body, "    mj_%[1]s = %[2]s_K.out.keys.join(io_%[1]s.map { ck, bdl -> tuple(Mro2nf.outerKey(ck), bdl) }.groupTuple(), remainder: true).map { ok, fk, so -> tuple(ok, so ?: [], fk) }\n", c.Name, fork)
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
	nulls := "${projectDir}/nulls/" + qualify(pipeline, c.Name)

	// Native gating (#59, Lever 2) for a self.<field> flag: read it from the
	// per-fork pipeline-args bundle (row[1]) — no keyed DISABLE task, no join.
	// (An upstream-ref keyed disable keeps the DISABLE_K bind: its position in the
	// keyed row depends on the bind's ref order.)
	if src, field, ok := nativeDisableGate(c); ok && src == "pa" {
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
	away := fusedAwayProducers(p, prog, g)

	for _, c := range p.Calls {
		// #59 Lever 4: a folded source emits no process; its consumer emits the
		// combined producer+consumer process instead of a plain fused stage.
		if away[c.Name] {
			continue
		}

		if prod, ps, cs, ok := chainFusion(c, p, prog, g); ok {
			genFusedChainProcess(b, p.Name, prod, c, ps, cs, g)

			continue
		}

		if c.Mapped {
			genForkBindProcess(b, p.Name, c, g)
			genMergeProcess(b, p.Name, c, calleeOutNames(prog, c.Callable), g)

			// A natively-gated disable (#59) needs no DISABLE process; the mapped
			// wiring reads the flag on the driver.
			if _, _, native := nativeDisableGate(c); c.Disabled != nil && !native {
				genDisableProcess(b, p.Name, c, g)
			}

			continue
		}

		// An emit-once forward routes the producer straight through, so it needs no
		// BIND process (genCallWiring emits the direct wiring instead).
		if _, ok := callForwardProducer(c, p, prog); ok {
			continue
		}

		// A fuseable non-split stage call runs `mre bind` inline in the stage task;
		// emit that fused process instead of a standalone BIND (#16).
		if s, ok := fuseableStageCall(c, p, prog); ok {
			genFusedStageProcess(b, p.Name, c, s, g)

			continue
		}

		// A fuseable split stage call folds bind into a per-call SPLIT that emits
		// the bound args for the aliased MAIN/JOIN (#16).
		if s, ok := fuseableSplitCall(c, p, prog); ok {
			genFusedSplitProcess(b, p.Name, c, s, g)
			genFusedSplitWorkflow(b, p.Name, c)

			continue
		}

		// A natively-disabled leaf-stage call fuses bind into its stage task; the
		// driver-side gate needs neither a BIND nor a DISABLE process (#59).
		if s, ok := fuseableDisabledStage(c, p, prog); ok {
			genFusedStageProcess(b, p.Name, c, s, g)

			continue
		}

		genBindProcess(b, bindName(p.Name, c.Name), c.Bindings, g, g.producerArgs(c.Callable, types.RoleIn))

		// A natively-gated disable (#59) reads the flag on the driver, so the plain
		// workflow needs no DISABLE process. A keyed pipeline still emits its keyed
		// DISABLE (genKeyedPipeProcesses) for the keyed workflow.
		if _, _, native := nativeDisableGate(c); c.Disabled != nil && !native {
			genDisableProcess(b, p.Name, c, g)
		}
	}

	// The return bind builds the pipeline's own output bundle, unless the returns
	// forward one call's outputs verbatim (then no BIND — routed directly).
	if _, ok := forwardProducer(p.Returns, p, prog); !ok {
		genBindProcess(b, bindName(p.Name, "return"), p.Returns, g, g.producerArgs(p.Name, types.RoleOut))
	}
}

func genStage(b *strings.Builder, s *ir.Stage, g genCtx) {
	code := g.code[s.Name]
	mainOuts := strings.Join(append(names(s.Out), names(s.ChunkOut)...), ",")
	joinOuts := strings.Join(names(s.Out), ",")
	base := g.stageCmd("main", code, s.Lang, vmemFlag(s, "main"))

	// The fork-keyed variants are only ever invoked for a stage reachable under a
	// map call; for any other stage they are dead process definitions, so emit
	// them only when needed (#59).
	keyed := g.keyed[s.Name]

	if !s.Split {
		genSingleStage(b, s, base, joinOuts, g)

		if keyed {
			genKeyedSingleStage(b, s, base, joinOuts, g)
		}

		return
	}

	genSplitProcesses(b, s, g, base, mainOuts, joinOuts)
	genSplitWorkflow(b, s)

	// A fork-key-threaded variant, used when this split stage is a map-call
	// target so each fork runs its own split/main/join and gathers per fork.
	if keyed {
		genKeyedSplitProcesses(b, s, g, base, mainOuts, joinOuts)
		genKeyedSplitWorkflow(b, s)
	}
}

// keyedReachable returns the set of stage AND pipeline names that can run under a
// map call — directly targeted by a `map call`, or reached through a (possibly
// nested) sub-pipeline that is map-called. Only these callables need a fork-keyed
// variant emitted (a stage's _MAP/_SPLIT_K processes, a pipeline's wf_<p>_map
// plus its keyed includes/processes); every other callable's keyed layer is
// never invoked. The analysis unions over all call paths, so a callable reached
// both keyed and plain is still marked keyed.
func keyedReachable(prog *ir.Program) map[string]bool {
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
			walk(c.Callable, keyed || c.Mapped)
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
func genKeyedSplitProcesses(b *strings.Builder, s *ir.Stage, g genCtx, base, mainOuts, joinOuts string) {
	splitCmd := g.stageCmd("split", g.code[s.Name], s.Lang, vmemFlag(s, "split"))
	joinCmd := g.stageCmd("join", g.code[s.Name], s.Lang, vmemFlag(s, "join"))

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
    chunks = %[1]s_SPLIT_K.out.chunks.flatMap { key, cs -> Mro2nf.keyedChunks(key, cs) }
    %[1]s_MAIN_K(chunks.combine(ch.mn, by: 0), types)
    joinres = %[1]s_SPLIT_K.out.joinres.map { key, f -> tuple(key, Mro2nf.parseJson(f)) }
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
    %[6]s
    path 'types.json'
  output:
    %[7]s
  script:
    """
    %[3]s -args args -outs '%[4]s'%[5]s -threads ${task.cpus} -memgb ${task.memory.toGiga()} -work . -o outs
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

`, s.Name, stageDirectives(s, ""), base, outs, g.producerArgs(s.Name, types.RoleMainOut),
		bundleInput("args"), bundleOutput("outs"))
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
	splitCmd := g.stageCmd("split", g.code[s.Name], s.Lang, vmemFlag(s, "split"))
	joinCmd := g.stageCmd("join", g.code[s.Name], s.Lang, vmemFlag(s, "join"))

	fmt.Fprintf(b, `process %[1]s_SPLIT {
%[2]s
  input:
    %[14]s
    path 'types.json'
  output:
    path 'chunks.json', emit: defs
    path 'joinres.json', emit: joinres
    path 'chunk_*', emit: chunks, type: 'dir', optional: true
  script:
    """
    %[4]s -args args -work . -o chunks.json -joinres joinres.json -chunkdir . -threads ${task.cpus} -memgb ${task.memory.toGiga()}%[9]s
    """
}

process %[1]s_MAIN {
%[12]s
  input:
    tuple val(res), path(chunk), %[15]s
    path 'types.json'
  output:
    path "out_${chunk.baseName}", type: 'dir'
  script:
    """
    %[5]s -args args -chunk ${chunk} -outs '%[6]s'%[10]s -threads ${task.cpus} -memgb ${task.memory.toGiga()} -work . -o out_${chunk.baseName}
    """
}

process %[1]s_JOIN {
%[13]s
  input:
    val join
    %[14]s
    path defs
    path souts
    path 'types.json'
  output:
    %[16]s
  script:
    """
    %[7]s -args args -chunkdefs ${defs} -chunkouts "\$(ls -1d out_* 2>/dev/null | sort -V | paste -sd, -)" -outs '%[8]s'%[11]s -threads ${task.cpus} -memgb ${task.memory.toGiga()} -work . -o outs
    """
}

`, s.Name, stageDirectives(s, ""), memOf(s), splitCmd, base, mainOuts, joinCmd, joinOuts,
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
    %[1]s_JOIN(join, a, %[1]s_SPLIT.out.defs, %[1]s_MAIN.out.collect().ifEmpty([]), types)
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
	var inputs strings.Builder

	// Each input bundle is reconstructed under its own dir (pipeargs/, in_<id>/)
	// from the staged sidecar + individual leaf items. The distinct dir names also
	// keep pipeargs from clobbering this process's own `-o args` output (bug 1).
	fmt.Fprintf(&inputs, "    %s\n", bundleInput("pipeargs"))

	pairs := make([]string, 0, len(refs))
	for _, id := range refs {
		fmt.Fprintf(&inputs, "    %s\n", bundleInput("in_"+id))
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
    %[6]s
  script:
    """
    '%[3]s' bind -spec 'spec.json' -pipeargs pipeargs%[4]s -o args%[5]s
    """
}

`, name, block, g.mre, arg, prodArgs, bundleOutput("args"))
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
    '%[3]s' forkbind -spec 'spec.json' -pipeargs pipeargs%[4]s -chunkdir . -mapmode %[6]s%[5]s
    """
}

`, forkName(pipeline, c.Name), block, g.mre, arg, g.producerArgs(c.Callable, types.RoleIn), mapModeArg(c))
}

// Map-call fork kinds — the ir.Call.MapMode values derived from Martian's
// CallMode (map_call_source.go).
const (
	mapModeMap     = "map"
	mapModeArray   = "array"
	mapModeUnknown = "unknown"
)

// mapModeArg is the static fork kind for a map call: "map" for a typed-map (or
// not-statically-resolved "unknown") source, else "array". It drives the
// fork/merge so an empty or null typed source resolves to the typed empty
// ([]/{}) instead of being sniffed from the runtime value (which mis-classifies
// null). "unknown" maps to "map" to stay consistent with forkDims (emit.go),
// whose output-projection treats an unknown mode as a keyed map.
func mapModeArg(c ir.Call) string {
	if c.MapMode == mapModeMap || c.MapMode == mapModeUnknown {
		return mapModeMap
	}

	return mapModeArray
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
    %[5]s
  script:
    """
    '%[2]s' merge -outs '%[3]s' -files "\$(ls -1d outs__* 2>/dev/null | sort -V | paste -sd, -)" -keys-file forkkeys.json -o merged%[4]s
    """
}

`, mergeName(pipeline, c.Name), g.mre, calleeOuts, g.producerArgs(c.Callable, types.RoleOut),
		bundleOutput("merged"))
}

func genPipelineWorkflow(b *strings.Builder, p *ir.Pipeline, prog *ir.Program, g genCtx) {
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
		genCallWiring(&body, p, prog, c, g)
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
		genCallWiring(&body, p, prog, c, g)
	}

	// The pipeline's own output: when its returns forward one call's outputs
	// verbatim, emit that producer's bundle directly instead of rebuilding it in a
	// return BIND (emit-once, #14); otherwise the return bind assembles it.
	emit := bindName(p.Name, "return") + ".out"
	if prod, ok := forwardProducer(p.Returns, p, prog); ok {
		emit = "ch_" + prod
	} else {
		fmt.Fprintf(&body, "    %s(%s)\n", bindName(p.Name, "return"), bindCallArgs(p.Returns, bindName(p.Name, "return")))
	}

	fmt.Fprintf(b, `workflow %s {
  take: pipeargs
%s  emit:
    %s
}

`, p.Name, body.String(), emit)
}

// partitionGateablePreflight splits calls into the gateable preflight calls
// (preflight, plain — not mapped/disabled — and bound only to pipeline inputs or
// literals) and everything else, each in original order. A gateable preflight
// depends on nothing but pipeargs, so it can run first and gate the rest without
// a cycle. A preflight that references another call is left in place (it cannot
// gate the pipeline it is downstream of) and keeps its prior in-order behavior.
func partitionGateablePreflight(calls []ir.Call) ([]ir.Call, []ir.Call) {
	var pre, rest []ir.Call

	for _, c := range calls {
		if c.Preflight && !c.Mapped && c.Disabled == nil && !bindingsRefCall(c.Bindings) {
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
			return v.Ref.Kind == refKindCall
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
func genCallWiring(b *strings.Builder, p *ir.Pipeline, prog *ir.Program, c ir.Call, g genCtx) {
	pipeline := p.Name
	callee := callAlias(pipeline, c.Name)

	// #59 Lever 4: a source stage folded into its consumer emits no wiring of its
	// own; the consumer runs the combined producer+consumer process off pipeargs.
	if g.fuseChains {
		if fusedAwayProducers(p, prog, g)[c.Name] {
			return
		}

		if prod, _, _, ok := chainFusion(c, p, prog, g); ok {
			fmt.Fprintf(b, "    ch_%s = %s(pa, types, %s, %s)\n", c.Name, fusedName(pipeline, c.Name),
				specFile(bindName(pipeline, prod.Name)), specFile(bindName(pipeline, c.Name)))

			return
		}
	}

	if c.Mapped {
		genMappedWiring(b, pipeline, c, callee)

		return
	}

	// Emit-once routing (epic #18 / #14): when a call's inputs are a verbatim
	// forward of one upstream call's outputs, feed that producer's output bundle
	// straight into the callee, skipping the BIND that would only re-materialize
	// its files. The producer's channel is a value channel, so it is reusable.
	if prod, ok := callForwardProducer(c, p, prog); ok {
		fmt.Fprintf(b, "    ch_%s = %s(ch_%s)\n", c.Name, callee, prod)

		return
	}

	// A fuseable non-split stage call invokes its fused bind+main process directly
	// with the same inputs a BIND would take, so the standalone BIND is gone (#16).
	if _, ok := fuseableStageCall(c, p, prog); ok {
		fmt.Fprintf(b, "    ch_%s = %s(%s)\n", c.Name, fusedName(pipeline, c.Name),
			bindCallArgs(c.Bindings, bindName(pipeline, c.Name)))

		return
	}

	// A fuseable split call invokes its per-call fused workflow (bind+split →
	// MAIN → JOIN); types and bindspec are resolved inside it (#16).
	if _, ok := fuseableSplitCall(c, p, prog); ok {
		fmt.Fprintf(b, "    ch_%s = %s(%s)\n", c.Name, fusedName(pipeline, c.Name),
			fusedSplitCallArgs(c.Bindings))

		return
	}

	// A natively-disabled leaf-stage call fuses its bind into the fused bind+main
	// process, gated by genFusedDisabledWiring — no standalone BIND (#59, Lever 3).
	if _, ok := fuseableDisabledStage(c, p, prog); ok {
		genFusedDisabledWiring(b, pipeline, c)

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
		if r == nil || r.Kind != refKindCall || r.Output != bnd.Param || strings.Contains(r.Output, ".") {
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

// chainFusion reports whether consumer call c is the downstream end of a linear
// chain that -fuse-chains folds into one task (#59, Lever 4): its single call-ref
// input is a *source* leaf stage (no call inputs of its own) that feeds only c,
// and both stages request identical resources — so the one fused task's
// -threads/-memgb are correct for both, keeping output byte-identical. It returns
// the producer call and both stages. Producers are always sources, so a fused
// producer is never itself a consumer end — no 3-stage chains, no conflicts.
func chainFusion(c ir.Call, p *ir.Pipeline, prog *ir.Program, g genCtx) (ir.Call, *ir.Stage, *ir.Stage, bool) {
	if !g.fuseChains {
		return ir.Call{}, nil, nil, false
	}

	cs, ok := fuseableLeaf(c, p, prog)
	if !ok {
		return ir.Call{}, nil, nil, false
	}

	refs := refCalls(c.Bindings)
	if len(refs) != 1 {
		return ir.Call{}, nil, nil, false
	}

	prod, ok := callByName(p, refs[0])
	if !ok {
		return ir.Call{}, nil, nil, false
	}

	ps, ok := fuseableLeaf(prod, p, prog)
	if !ok || len(refCalls(prod.Bindings)) != 0 || consumerCount(prod.Name, p) != 1 {
		return ir.Call{}, nil, nil, false
	}

	if ps.Resources != cs.Resources {
		return ir.Call{}, nil, nil, false
	}

	return prod, ps, cs, true
}

// fuseableLeaf is fuseableStageCall restricted to non-preflight calls (a
// preflight's gate wiring makes it unfit as a chain end).
func fuseableLeaf(c ir.Call, p *ir.Pipeline, prog *ir.Program) (*ir.Stage, bool) {
	if c.Preflight {
		return nil, false
	}

	return fuseableStageCall(c, p, prog)
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
func fusedAwayProducers(p *ir.Pipeline, prog *ir.Program, g genCtx) map[string]bool {
	if !g.fuseChains {
		return nil
	}

	away := map[string]bool{}

	for _, c := range p.Calls {
		if prod, _, _, ok := chainFusion(c, p, prog, g); ok {
			away[prod.Name] = true
		}
	}

	return away
}

// genFusedChainProcess emits one process running producer then consumer inline:
// bind+main for the source, then bind+main for the consumer with the source's
// outputs fed in locally (#59, Lever 4). Both use the consumer's directives (the
// resources are equal, per chainFusion).
func genFusedChainProcess(b *strings.Builder, pipeline string, prod, cons ir.Call, ps, cs *ir.Stage, g genCtx) {
	pbase := g.stageCmd("main", g.code[ps.Name], ps.Lang, vmemFlag(ps, "main"))
	cbase := g.stageCmd("main", g.code[cs.Name], cs.Lang, vmemFlag(cs, "main"))

	fmt.Fprintf(b, `process %[1]s {
%[2]s
  input:
    %[3]s
    path 'types.json'
    path 'spec_prod.json'
    path 'spec_cons.json'
  output:
    %[4]s
  script:
    """
    '%[5]s' bind -spec 'spec_prod.json' -pipeargs pipeargs -o args_prod%[6]s
    %[7]s -args args_prod -outs '%[8]s'%[9]s -threads ${task.cpus} -memgb ${task.memory.toGiga()} -work . -o outs_prod
    '%[5]s' bind -spec 'spec_cons.json' -pipeargs pipeargs -inputs %[10]s=outs_prod -o args_cons%[11]s
    %[12]s -args args_cons -outs '%[13]s'%[14]s -threads ${task.cpus} -memgb ${task.memory.toGiga()} -work . -o outs
    """
}

`, fusedName(pipeline, cons.Name), stageDirectives(cs, ""), bundleInput("pipeargs"),
		bundleOutput("outs"), g.mre,
		g.producerArgs(prod.Callable, types.RoleIn), pbase, strings.Join(names(ps.Out), ","),
		g.producerArgs(prod.Callable, types.RoleMainOut), prod.Name,
		g.producerArgs(cons.Callable, types.RoleIn), cbase, strings.Join(names(cs.Out), ","),
		g.producerArgs(cons.Callable, types.RoleMainOut))
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
	splitCmd := g.stageCmd("split", g.code[s.Name], s.Lang, vmemFlag(s, "split"))

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
    %[5]s(join, a, %[3]s.out.defs, %[4]s.out.collect().ifEmpty([]), types)
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
// consumes it — folding the standalone BIND away (#16).
func genFusedStageProcess(b *strings.Builder, pipeline string, c ir.Call, s *ir.Stage, g genCtx) {
	block, arg := bindInputs(refCalls(c.Bindings))
	base := g.stageCmd("main", g.code[s.Name], s.Lang, vmemFlag(s, "main"))
	outs := strings.Join(names(s.Out), ",")

	fmt.Fprintf(b, `process %[1]s {
%[2]s
  input:
%[3]s  output:
    %[8]s
  script:
    """
    '%[4]s' bind -spec 'spec.json' -pipeargs pipeargs%[5]s -o args%[9]s
    %[6]s -args args -outs '%[7]s'%[10]s -threads ${task.cpus} -memgb ${task.memory.toGiga()} -work . -o outs
    """
}

`, fusedName(pipeline, c.Name), stageDirectives(s, ""), block, g.mre, arg, base, outs,
		bundleOutput("outs"), g.producerArgs(c.Callable, types.RoleIn),
		g.producerArgs(c.Callable, types.RoleMainOut))
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
	// (collect() on an empty channel emits nothing); MERGE then yields the typed
	// empty ([] for an array fork, {} for a map fork).
	// FORK.out.keys carries map-fork keys (null for an array fork).
	fmt.Fprintf(b, "    %s(out_%s.collect().ifEmpty([]), %s.out.keys, types)\n", merge, c.Name, fork)

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
func genMappedDisableGate(b *strings.Builder, pipeline string, c ir.Call) string {
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

			return bindCallArgsPa(c.Bindings, fmt.Sprintf("g_%s.run.map { data, leaves -> tuple(data, leaves) }", c.Name), bind)
		}

		fmt.Fprintf(b, `    g_%[1]s = pa.combine(%[2]s).branch { data, leaves, gd, gl ->
        def off = Mro2nf.disabledField(gd, '%[3]s')
        run: !off
        skip: off
    }
`, c.Name, src, field)

		return bindCallArgsPa(c.Bindings, fmt.Sprintf("g_%s.run.map { data, leaves, gd, gl -> tuple(data, leaves) }", c.Name), bind)
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
	return bindCallArgsPa(c.Bindings, fmt.Sprintf("g_%s.run.map { data, leaves, d -> tuple(data, leaves) }", c.Name), bind)
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
    s_%[1]s = g_%[1]s.skip.map { data, leaves, d -> %[6]s }
    ch_%[1]s = r_%[1]s.mix(s_%[1]s).first()
`, c.Name, bind, dis, callee, nulls, nullBundle(nulls))
}

// genFusedDisabledWiring gates a natively-disabled leaf-stage call and feeds the
// enabled forks straight into the fused bind+main process — no standalone BIND
// (#59, Lever 3). The self case branches on pa (the flag lives in the fork args);
// the upstream-ref case combines pa with the producing channel to read the flag.
func genFusedDisabledWiring(b *strings.Builder, pipeline string, c ir.Call) {
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
`, c.Name, field, fused, bindCallArgsPa(c.Bindings, enabled, bind), nullBundle(nulls))

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
`, c.Name, src, field, fused, bindCallArgsPa(c.Bindings, enabled, bind), nullBundle(nulls))
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
	case refKindSelf:
		// self.<field>: the whole referenced input is the flag (no sub-path).
		if r.Output == "" {
			return "pa", r.ID, true
		}
	case refKindCall:
		// CALL.out.<field>: a single (non-nested) output field.
		if r.Output != "" && !strings.Contains(r.Output, ".") {
			return "ch_" + r.ID, r.Output, true
		}
	}

	return "", "", false
}

// keyedNativeDisable reports whether a mapped call's keyed disable gate reads the
// flag natively (a self.<field> flag from the per-fork args) — the only keyed
// case genKeyedMappedDisableGate gates without a DISABLE_K bind.
func keyedNativeDisable(c ir.Call) bool {
	src, _, ok := nativeDisableGate(c)

	return ok && src == "pa"
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

// genPublish emits the terminal publish as two processes that avoid a single-node
// funnel (#12): LAYOUT stages ONLY the final sidecar (data.json) — no file leaves
// — to compute the outs/ layout (leaf basename -> outs/ rel path) and the
// pipeline_outs.json value tree; PUBLISH_LEAF then stages each leaf individually
// and publishes it into outs/<rel>, so the result set is published in parallel
// across tasks rather than round-tripped through one node. The physical outs/
// tree (which downstream pipelines consume) is unchanged.
func genPublish(b *strings.Builder, entry string, g genCtx) {
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
    %[10]s
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
  // Publish without a single-node funnel (#12): LAYOUT reads only the sidecar to
  // compute the outs/ layout; PUBLISH_LEAF publishes each leaf into place in
  // parallel. multiMap splits the final output tuple so both consume it safely.
  ep = %[5]s.out.multiMap { s, l -> side: s; leaves: l }
  LAYOUT(ep.side, types)
  lmap = LAYOUT.out.layout.map { f -> Mro2nf.parseJson(f) }
  leaves = ep.leaves.flatMap { l -> (l instanceof List ? l : [l]).collect { leaf -> tuple(leaf.name, leaf) } }
  PUBLISH_LEAF(leaves.combine(lmap).flatMap { base, leaf, m -> (m[base] ?: []).collect { rel -> tuple(rel, leaf) } })
}
`, decls.String(), g.mre, prog.Entry.Callable, valuesMap, entryWorkflow,
		fileInputs.String(), flatFlag, fileChans.String(), strings.Join(callArgs, ", "),
		bundleOutput("entry_resolved"))
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
		if v.Ref != nil && v.Ref.Kind == refKindCall && !seen[v.Ref.ID] {
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
