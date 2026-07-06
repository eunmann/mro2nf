package emit

import (
	"fmt"
	"strings"

	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/types"
)

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

		// Sort by UTF-8 byte order (Mro2nf.compareUtf8), NOT Groovy's natural
		// UTF-16 order: mre's entryargs re-walks the same map with Go sort.Strings
		// (UTF-8 bytes), and the two orders diverge for supplementary-plane keys,
		// which would mispair the flattened file leaves with the staged paths.
		return fmt.Sprintf("(%s ?: [:]).sort { __a, __b -> Mro2nf.compareUtf8(__a.key, __b.key) }.collect { %s -> %s }.flatten()", expr, v, fileFlattenExprDepth(v+".value", val, structs, depth+1))
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
