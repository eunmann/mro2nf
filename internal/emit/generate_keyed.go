package emit

import (
	"fmt"
	"strings"

	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/types"
)

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
