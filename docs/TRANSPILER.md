# How the Martian → Nextflow transpiler works

This document explains, end to end, how `mro2nf` turns a Martian
pipeline (`.mro`) into a runnable Nextflow project — what each piece does, how
data flows, and why the design is shaped the way it is. It is meant to be read
top to bottom by someone who knows neither codebase deeply.

If you want the per-feature fidelity table (what is faithful vs. divergent), see
[`FEATURE_COVERAGE.md`](FEATURE_COVERAGE.md). For the live-cloud validation
record, see [`LIVE_AWS_TEST.md`](LIVE_AWS_TEST.md).

---

## 1. The one big idea: translate the orchestration, reuse the stage code

A Martian pipeline is two things stacked together:

1. **Orchestration** — the DAG of stages, how they fan out into parallel
   *chunks* (`split`) and *forks* (mapping a call over an array/map), how outputs
   of one stage bind to the inputs of the next, and how much CPU/RAM each piece
   asks for.
2. **Stage code** — the actual Python/exec/compiled programs that do the work,
   driven through Martian's *adapter ABI* (the contract Martian uses to hand a
   stage its inputs and collect its outputs).

This transpiler translates **only the orchestration** into Nextflow. It does
**not** rewrite or reimplement your stage code. Instead, every Nextflow process
it generates runs the **original, unmodified** Martian stage through a small Go
shim called **`mre`**, which speaks the exact same adapter ABI that Martian's own
runner (`mrp`) speaks. The stage can't tell the difference.

This one decision shapes everything else:

- **Correctness.** The stage code runs through the real Martian adapter
  (`martian_shell.py`), so its outputs are byte-for-byte what `mrp` would
  produce. The transpiler only has to schedule the same work in the same shape.
  It never re-derives the science.
- **Two programs.** `mro2nf` is the transpiler; it runs at build time and emits the
  Nextflow project. `mre` is the runtime shim, baked into that project; it runs
  each stage phase at execution time.
- **A neutral data plane.** Martian passes data between stages as JSON plus files
  on a shared filesystem. On the cloud, Nextflow has no shared filesystem: each
  task is an isolated container with an object-store (S3) work dir. So the
  transpiler defines a self-contained *bundle* format that carries a stage's
  inputs and outputs (the JSON and the files it references) as one directory
  Nextflow can stage between tasks. This is what lets the same project run
  identically on a laptop, an HPC cluster, AWS Batch + S3, and AWS HealthOmics.

```
          build time (mro2nf)                         run time (Nextflow + mre)
   ┌──────────────────────────┐            ┌──────────────────────────────────┐
   │  pipeline.mro            │            │  process SUM_SQUARES_SPLIT { … }  │
   │      │  parse + lower     │   emits    │     └─ runs:  mre split …         │
   │      ▼                    │  ───────►  │  process SUM_SQUARES_MAIN  { … }  │
   │  IR (Program)            │            │     └─ runs:  mre main …          │
   │      │  emit              │            │  process SUM_SQUARES_JOIN  { … }  │
   │      ▼                    │            │     └─ runs:  mre join …          │
   │  Nextflow project        │            │     each mre call drives the real │
   └──────────────────────────┘            │     Martian stage via the adapter │
                                           └──────────────────────────────────┘
```

---

## 2. The pipeline at a glance

```
.mro file
   │
   │  cmd/mro2nf  (the `mro2nf` CLI)
   ▼
frontend.Parse   → Martian AST     (uses github.com/martian-lang/martian/syntax)
   │
frontend.Lower   → IR (internal/ir)  — a normalized, transpiler-friendly model
   │
emit.Emit        → a Nextflow project on disk:
   │                   main.nf, modules/*.nf, nextflow.config,
   │                   _assets/ (types.json + bindspecs), entry_args/
   │                   (+ Dockerfile / packaging for cloud targets)
   ▼
nextflow run main.nf …   →  processes invoke the baked `mre` shim, which
                            drives the original Martian stage code.
```

Source layout (from `CLAUDE.md`):

```
cmd/mro2nf/      → transpiler CLI: .mro -> Nextflow project
cmd/mre/       → runtime shim: runs one stage phase (split|main|join|…) per process
internal/
  frontend/    → parse .mro via martian/syntax, lower to IR
  ir/          → the normalized transpiler IR
  emit/        → IR -> Nextflow (.nf + nextflow.config) templates
  types/       → type-directed file-leaf walk (shared by emit + shim)
  shim/        → bundle I/O, path rewrite, Martian adapter launch
  bind/        → resolve call bindings into _args (refs, projections, fan-in)
  overrides/   → mrp --overrides file -> Nextflow -c config
  config/      → .mro2nf.yml project defaults for the transpiler flags
  logging/     → zerolog setup (stderr)
  apperror/    → sentinel + typed errors
```

---

## 3. Front end: `.mro` → IR

The transpiler does not re-implement the Martian grammar. It parses the `.mro`
with the upstream `github.com/martian-lang/martian/syntax` package
(`frontend.Parse`), then **lowers** that AST into a small, normalized
intermediate representation (`frontend.Lower` → `internal/ir`). The IR exists so
the emitter never has to think about Martian's surface syntax — only about a flat
set of stages, pipelines, calls, and bindings. It then resolves the invocation's
call graph with the same `syntax` function `mrp` runs before it creates a
pipestance (`frontend.CheckCallGraph`), so a pipeline `mrp` refuses to start is
refused at transpile time with `mrp`'s message.

The `mro2nf` CLI (`cmd/mro2nf/main.go`) ties this together:

- Flags: `-o <dir>` (output project), `-mre`/`-shell`/`-mrjob` (paths to the
  runtime shim, the Martian Python adapter, and the optional compiled-stage
  wrapper, all baked into the generated scripts), `-mropath` (search path for
  `@include`), `-target` (`local` | `awsbatch` | `healthomics`), `-container`
  (image URI for cloud targets), `-monitor` (per-stage memory enforcement),
  the opt-in emission mode `-native` (§4.8), `-allow-external-files` (§8,
  §10), `-config` (a `.mro2nf.yml` of per-project flag defaults; keys mirror the flags, an
  explicit flag always wins — see `internal/config`; `-allow-external-files`
  and `-mropath` have no key, so a config file shipped beside a shared `.mro`
  cannot set them, and a tool path it sets is baked into an image only from
  inside the roots, §10),
  and `-version`.
- It resolves the stage source paths and the runtime tool paths to **absolute**
  paths (so the generated project runs from any working directory), records the
  `.mro`'s directory as `MRODir` (used to resolve relative file defaults — see
  §8) and the `-mropath` roots as `MROPath`, and calls `emit.Emit(prog, opts)`.

The IR (`internal/ir/ir.go`) is the contract between front end and emitter. Its
core types:

- **`Program`** — the whole pipeline: `Stages`, `Pipelines`, `Structs` (type
  table), and `Entry` (the top-level `call`).
- **`Stage`** — a stage declaration: inputs/outputs (`[]Param`), whether it has a
  `split` (and the chunk in/out params), its `Resources`, and its source
  language + path.
- **`Pipeline`** — inputs/outputs plus an ordered list of `Call`s and the
  pipeline's `return` bindings.
- **`Call`** — one call site: the callee name, its `Bindings`, and modifiers
  (`Mapped` for a map/fork call, `Disabled`, `Local`, `Preflight`, `Volatile`).
- **`Binding` / `Value`** — how an input is wired: a literal, a reference to
  another call's output (`Ref`), an array/map literal, or a `split`/sweep marker.
- **`Param`** — one typed input/output: `Name`, `Type`, `BaseType`, `ArrayDim`,
  `MapDim`, `IsFile`, and (outputs) `OutName`. These drive the type-directed
  file-leaf walk in §7.
- **`StructType`** — a Martian `struct`: an ordered list of typed fields.

---

## 4. Emit: mapping every Martian construct to Nextflow

The emitter (`internal/emit`) writes a complete Nextflow DSL2 project. The shape
of the output is the heart of the transpiler, so it helps to see a concrete
example. For a simple pipeline that calls a split stage `SUM_SQUARES` then
`REPORT`, the project is:

```
main.nf                              ← entry workflow + LAYOUT/PUBLISH_LEAF + BUILD_ENTRY_ARGS
modules/
  stage_SUM_SQUARES.nf               ← one module per stage
  stage_REPORT.nf
  pipe_SUM_SQUARE_PIPELINE.nf        ← one module per pipeline
nextflow.config                      ← executor profiles, retry policy, params
lib/Mro2nf.groovy                    ← driver-side channel helpers (see below)
_assets/
  types.json                         ← the program's type manifest
  bindspecs/BIND_*.json              ← one binding spec per call (see §6)
entry_args/data.json                 ← the baked top-level call arguments (§8)
```

The non-trivial channel logic the workflows need — reading a bundle's sidecar,
resolving a disable gate, expanding a keyed SPLIT into per-chunk tuples,
enumerating forks from `forknames.json`, stripping a composite fork key — lives
in a hand-written **`lib/Mro2nf.groovy`** (`@CompileStatic`, `java.nio.Path`
APIs only), shipped verbatim into every project (embedded in the transpiler, same
as `_assets/`). Nextflow auto-adds `lib/` to the driver classpath, so generated
code calls `Mro2nf.disabledDir(d, 'disabled')`, `Mro2nf.chunkRes(c)`, `Mro2nf.forkTuples(ok, d)`
etc. instead of stamping a dense one-line closure at each site. Every helper runs
on the **driver** while wiring channels — never in a task — so it changes nothing
for AWS Batch / S3 / HealthOmics execution, and the file rides along in the
HealthOmics zip like `_assets/` and `nulls/`.

### 4.1 A plain stage → a process

A non-split stage becomes a single Nextflow process whose `script` block runs
`mre main`, wrapped in a `wf_<stage>` subworkflow:

```groovy
process SOMESTAGE {
  cpus 1
  memory { 1 * task.attempt + ' GB' }        // grows on retry (OOM escalation)
  input:
    tuple(path('args/data.json'), path('args/f/*'))   // input bundle: sidecar + leaves
    path 'types.json'          // the shared type manifest (staged; see §9)
  output:
    tuple(path("outs/data.json"), path("outs/f/*", arity: '0..*'))
  script:
    """
    'mre' main … -args args -outs 'y' … -o outs
    """
}
workflow wf_SOMESTAGE {
  take: args
  main:
    types = file("${projectDir}/_assets/types.json")
    SOMESTAGE(args, types)
  emit:
    SOMESTAGE.out
}
```

The process consumes and emits a **bundle** — a typed JSON sidecar
(`data.json`) plus its file leaves as individual `path` items, which Nextflow
stages back into one directory (`args/`, `outs/`) inside the task. Bundles are
the universal currency between processes (§7).

### 4.2 A split stage → SPLIT / MAIN / JOIN

Martian's `split` runs a stage in three phases — split the work into chunks, run
each chunk, join the results. The emitter renders three processes and wires them
in `wf_<stage>`:

```groovy
process SUM_SQUARES_SPLIT { … script: "mre split … -o chunks.json -joinres joinres.json -chunkdir ." }
process SUM_SQUARES_MAIN  { … script: "mre main  … -chunk ${chunk} -o out_${chunk.baseName}" }
process SUM_SQUARES_JOIN  { … script: "mre join  … -chunksfile chunks.json -chunkdir . -o outs" }

workflow wf_SUM_SQUARES {
  take: args
  main:
    types = file("${projectDir}/_assets/types.json")
    SUM_SQUARES_SPLIT(args, types)
    // each chunk dir carries its own resource request in data.json; the Mro2nf
    // helper reads it (raw-carriage, object-store-safe) and tuples it with the chunk:
    chunks = SUM_SQUARES_SPLIT.out.chunks.flatten().map { f -> tuple(Mro2nf.chunkRes(f), f) }
    SUM_SQUARES_MAIN(chunks.combine(args), types)
    join = SUM_SQUARES_SPLIT.out.joinres.map { f -> tuple(Mro2nf.parseJson(f), f) }
    // JOIN gathers the chunk outs sorted by name — NOT .collect(), whose
    // completion order would be part of JOIN's -resume cache key and would
    // starve a 0-chunk JOIN. toSortedList emits [] on an empty channel.
    SUM_SQUARES_JOIN(join, args, SUM_SQUARES_SPLIT.out.chunks.ifEmpty([]),
        SUM_SQUARES_SPLIT.out.defs,
        SUM_SQUARES_MAIN.out.toSortedList { x, y -> x.name <=> y.name }, types)
  emit:
    SUM_SQUARES_JOIN.out
}
```

Key points:

- **Data-dependent fan-out.** `mre split` runs your stage's `split()` function,
  which can read the input files and decide the chunk count **and** each chunk's
  resources at runtime. The emitter doesn't know the count; it `flatten()`s
  whatever chunk directories SPLIT produced and maps over them. (Live-verified:
  a 5-line input manifest fans out 5 chunks, a 3-line one fans out 3.)
- **Per-chunk resources.** Each chunk dir's `data.json` carries a `resources`
  object (`__threads`/`__mem_gb`). The MAIN process's `cpus`/`memory` are dynamic
  closures reading that object, so each chunk is provisioned per its request.
  A resource key of the wrong JSON type in a chunk or the join override fails
  the split with an error, as it does under `mrp`.
- **Split-returned join override.** A `split()` may also return a `join`
  resource request; SPLIT writes it to `joinres.json`, the workflow parses it
  into a `join` value, and JOIN's `cpus`/`memory` read it — so the join phase is
  scheduled like `mrp` would. JOIN also stages the file, and `mre join
  -joinres` overlays it on the stage's request for the `_args`/`_jobinfo` the
  stage sees.
- **What a stage sees vs. what the task gets.** Every phase passes `mre` the
  stage's Martian request (`-threads`/`-memgb`/`-vmemgb`/`-special`), which the shim
  resolves exactly as mrp does; the `cpus`/`memory` directives only size the
  task (whole units, escalating on retry) and never reach `_args`.
- **Chunk gather.** JOIN stages the `chunk_N` def bundles, the `out_chunk_N`
  outs, and the split's `chunks.json`. `mre join` lists the task dir itself,
  orders both sets by chunk index, and fails unless each numbers exactly the
  chunks in `chunks.json` — a lost chunk never joins as a shorter result.

### 4.3 A pipeline → a workflow, calls → BIND + callee

A pipeline becomes a `workflow <Pipeline>` whose body wires each call. Two pieces
per call:

1. A **BIND process** (`mre bind`) that resolves the call's input *bindings*
   (literals, `self.x`, and upstream `CALL.output` references) into the callee's
   input bundle.
2. The **callee invocation** (`wf_<callee>(BIND.out)`).

```groovy
workflow SUM_SQUARE_PIPELINE {
  take: pipeargs
  main:
    pa = pipeargs
    types = file("${projectDir}/_assets/types.json")
    BIND_…__SUM_SQUARES(pa, types, file("${projectDir}/_assets/bindspecs/BIND_…__SUM_SQUARES.json"))
    ch_SUM_SQUARES = wf_…__SUM_SQUARES(BIND_…__SUM_SQUARES.out)
    BIND_…__REPORT(pa, ch_SUM_SQUARES, types, file("…/BIND_…__REPORT.json"))
    ch_REPORT = wf_…__REPORT(BIND_…__REPORT.out)
    BIND_…__return(pa, ch_SUM_SQUARES, types, file("…/BIND_…__return.json"))
  emit:
    BIND_…__return.out         // the pipeline's own output bundle
}
```

A bind process takes the pipeline-args bundle plus one staged bundle per
referenced upstream call (`path 'in_<callid>'`), and runs `mre bind -spec
spec.json …` to produce the callee's args bundle. The `return` bind builds the
pipeline's output bundle the same way.

Two optimizations remove standalone BINDs where they would be pure plumbing:

- **Fold BIND into the stage task (#16).** A plain (non-mapped, enabled,
  non-preflight) stage call runs `mre bind` at the head of the *same* task as
  its stage phase — a fused per-call process named
  `STAGE_<n>_<pipeline>__<call>` (for a split stage, a fused bind+split process
  plus `_SP`/`_MN`/`_JN`-aliased phase processes). The call's referenced files
  stage into that one task once, instead of into a BIND task and again into the
  stage task.
- **Emit-once forward (#14).** When a call's bindings (or the pipeline's
  returns) forward one upstream call's entire output bundle verbatim, each
  under its producer's exact type and no struct among them, no process is
  emitted at all: the producer's output channel is routed straight to the
  consumer. A struct-typed forward keeps its BIND, since mrp filters every
  bound struct to its declared members.

### 4.4 Map / fork calls → FORK + MERGE (and keyed variants)

When a call is *mapped* (run once per element of an array/map input — Martian's
fork), the emitter renders:

- A **FORK process** (`mre forkbind`) that resolves the bindings into **one args
  bundle per fork**, written into a `forks/` directory, with a `forknames.json`
  listing the fork keys.
- The callee run **once per fork** through a **fork-key-threaded** variant of the
  callee workflow (`wf_<callee>_map`), where every channel item is a
  `tuple(key, bundle)` so chunks/joins stay partitioned by fork.
- A **MERGE process** (`mre merge`) that gathers the per-fork outputs back into a
  single array/map-shaped output bundle. `mre merge -forksdir <dir>` lists the
  staged `outs__<key>` bundles itself, orders them by the fork index each key
  ends in, and fails unless they number exactly the `count` in the
  `forkmeta.json` sidecar (`{"count": N, "keys": [...] | null}`) — no fork
  list rides on the command line, so the gather has no argv-length ceiling.
  A `"pruned": true` field marks a split of a statically null value (below),
  which merges zero forks to `null` as `mrp` prunes it (#436).
- For a map call over a sub-pipeline under a runtime disable gate, a
  **NULLFORKS process** on the gate's skip branch (`mre forkbind -keysonly`,
  then `mre merge -nullforks`): `mrp` disables such a call fork by fork, so its
  result is one `null` per fork. The merged bundle carries that whole-call
  value under the `@nullforks` member, which the binder returns for a whole
  ref (#434).
  The sidecar's file name is part of its format: a `-resume` across a format
  change finds no cached sidecar under the new name and re-runs the producer,
  so a merge never reads a stale-format one.

Every stage and pipeline therefore gets **two** emitted workflows: the plain
`wf_<name>` and the keyed `wf_<name>_map`. The keyed forms exist so a map call
over a split stage (or over a sub-pipeline that itself maps — *nested* maps) keys
every chunk and join by its fork. Fork directories are enumerated by **reading
the staged `forknames.json`** (via `.resolve()`), never `java.io.File.listFiles()`
— the latter cannot list an S3 work dir.

**Fork-independent calls leave the map call (#433).** `mrp` forks a stage only
over the map calls its inputs and disable read, and runs one with none once
(`fork0`), whatever the fork count. Before planning, `LiftForkIndependent`
(`internal/emit/lift.go`) moves each call of a map-called sub-pipeline that
reads none of the map call's split inputs (directly, or through another such
call) into the caller as `<mapcall>_<call>`, through plain and nested
sub-pipeline calls alike, and hands the residual copy (`<SUB>_lift<n>`) the
lifted calls' outputs as broadcast inputs (`__h_<call>`). Because `mrp` drops
a map call over a statically empty or null source with its children, and
disables them with it, the lifted calls are disabled by a **GATE** call
(`mre gate`, one task per lift, not per fork) that reads the map call's split
bindings (unless they are literals, never empty) and disable flag; a lifted
call with its own gate gets a gate of both. A call reading an outer map
call's element stays in that outer fork.

### 4.5 Disabled calls → a runtime gate

A `call … using (disabled = …)` is resolved at runtime: a **DISABLE process**
(`mre bind … -o disable`) computes the boolean, and the workflow `branch`es on it
— running the callee only for the enabled case and emitting a pre-baked
*null* bundle (from `nulls/<call>`) for the skipped case. Its whole payload is
JSON `null`, as mrp resolves a disabled call: a whole-call ref (`s = CALL`) and
every projection into it (`CALL.out`) both bind null. Disabled map calls gate
per fork.

### 4.6 Resources → process directives

- Static `using (mem_gb=…, threads=…)` → `cpus`/`memory` directives on the
  process.
- Per-chunk and split-returned-join resources → the dynamic `cpus`/`memory`
  closures shown in §4.2.
- Martian's `special` scheduler key → a `clusterOptions` directive that looks the
  key up in `params.job_resources` (the `MRO_JOBRESOURCES` analog; empty by
  default, populated per deployment for grid executors).

### 4.7 Outputs → LAYOUT + PUBLISH_LEAF

The entry pipeline's final outputs are published without a single-node funnel
(#12) by a pair of processes. **LAYOUT** stages only the final bundle's sidecar
(`data.json`) and runs `mre publish-layout`, which walks the output type and
computes the mrp-style `outs/` layout — every file output named by Martian's
`GetOutFilename` rule, arrays/maps/structs nested into index/key/field
subdirectories — plus the human-readable `pipeline_outs.json` and a compressed
machine-readable index, `manifest.json.gz` (#11). **PUBLISH_LEAF** then stages
each file leaf individually and publishes it to its `outs/` path via
`publishDir saveAs`, so the result set lands in parallel across tasks. The
published tree matches an mrp pipestance `outs/` (verified by
`the Go TestMrpDiff suite`); see §10 for where "results" lives on each target.

### 4.8 Opt-in emission mode: channel-native orchestration (`-native`)

Everything in §§4.1–4.7 describes the default emission. The opt-in `-native`
flag trades some launch-time flexibility for a smaller task graph. It is
**plan-decided**: `buildPlan` (`internal/emit/plan.go`) resolves every call to a
single `callKind` — plain bind, mapped, forward, one of the fused kinds, or
native scatter — once, up front, and the process, wiring, and include
emitters all read that fixed plan, so no two emission sites can disagree about
how a call is rendered.

- **`-native`** replaces the data-plane *tasks* with driver-side channel wiring,
  without touching the stage tasks or the adapter ABI:
  - **Baked entry args.** The entry bundle is resolved once at transpile time by
    running the same `mre entryargs` the `BUILD_ENTRY_ARGS` task would
    (`bakeEntryArgs` in `internal/emit/emit.go`), and the workflow stages
    `entry_resolved/` directly. Launch-time entry parameters are rejected with a
    hard error in the generated `main.nf` — re-transpile to change an input.
  - **The O(1) element scatter.** An eligible map call (a single whole-field
    self/upstream split feeding a value-typed param of an undisabled leaf
    stage) becomes `kindNativeScatter`: no FORK task at all. The driver parses
    the fork collection **once** (`Mro2nf.forkElements*` in
    `internal/emit/lib/Mro2nf.groovy`) and hands each fused forkbind+main
    instance only its own pre-sliced element, so per-instance work is O(1) and
    ordering matches the Go binder's canonical order byte-for-byte.
  - **Merge folds.** When a scattered (or, under `-native`, any mapped) call
    has exactly one consumer, its MERGE gather runs inline at the head of that
    consumer's task (`mergeFoldable`): the consumer stages the per-fork outs
    plus a keys sidecar and reconstructs the merged bundle with `mre merge`
    before its own bind — no standalone MERGE process.
  - **The keyed layer.** A map over a split stage or sub-pipeline still runs
    the fork-keyed workflow variants of §4.4; a value-only *nested* map inside
    a keyed pipeline collapses its per-outer-fork FORK_K resolve into a single
    driver element scatter (the `_KS` process, `Mro2nf.forkElementsKeyed`),
    keeping only the data-proportional MERGE_K gather.
  - **Empty-fork fidelity.** mrp's static resolver prunes an invocation-known
    empty fork to null while a runtime-empty collection merges to the typed
    empty; the `knownInvocation` analysis in `known.go` reproduces that split as
    a runtime shape rule, so entry inputs stay overridable in default mode. It
    is decided per call site: a sub-pipeline called from sites that need
    different marks is emitted once per context (`SplitKnownContexts`, copies
    named `<pipeline>_ctx<n>`), which adds no tasks.

**Detect and refuse, never under-deliver.** Every transpile runs the pre-emit
diagnostics (`internal/emit/diagnostics.go`) over the *same plan* that is
emitted. A flag/pipeline combination that would produce a wrong project is a
hard error that aborts the transpile; a shape an opt-in flag cannot fully
collapse prints an **Info** diagnostic naming exactly which orchestration
remains and why (e.g. which map call keeps its one FORK resolve) — an opt-in
never silently does less than asked.

---

## 5. The runtime shim `mre`

`mre` is a single Go binary with subcommands, one per phase a process needs. Each
subcommand reads bundle(s), drives the real Martian adapter for that phase, and
writes an output bundle:

- **`mre split`** — runs the stage's `split()` to produce chunk definitions
  (`chunks.json`), one chunk bundle per definition (each carrying its
  `resources`), and any split-returned `join` resources (`joinres.json`).
- **`mre main`** — runs the stage's `main()` for a whole stage (non-split) or for
  one chunk (`-chunk`), producing an output bundle.
- **`mre join`** — runs the stage's `join()` over the chunk outputs, producing the
  stage's final output bundle. Like a whole-stage `mre main`, it first checks
  those final outputs as mrp's output validation does (every declared output
  present and of its declared type) and fails the task on a violation.
- **`mre bind` / `mre forkbind`** — resolve a call's input *bindings* (from a
  bindspec + the staged upstream bundles + pipeline args) into the callee's args
  bundle. `forkbind` produces one bundle per fork.
- **`mre merge`** — gather per-fork output bundles into one array/map output.
- **`mre gate`** — evaluate a transpiler gate (§4.4, #433) into a bundle whose
  `off` field disables the calls lifted out of a map call: true when a gate
  flag is true or the map call's split bindings resolve to the zero forks
  `mrp` prunes statically.
- **`mre entryargs`** — build the top-level args bundle from the baked defaults
  overlaid with launch-time run parameters (§8).
- **`mre publish-layout`** — compute the mrp-style `outs/` layout from the final
  sidecar alone (no file leaves staged), writing `layout.json`,
  `pipeline_outs.json`, and `manifest.json.gz` for the PUBLISH_LEAF fan-out
  (§4.7).

Under the hood each phase hands the stage to the **real Martian adapter** —
`martian_shell.py` for `py` stages, an exec wrapper for `exec` stages, and the
`mrjob` wrapper for compiled (`comp`) stages — using the same on-disk ABI Martian
uses (`_args`, `_jobinfo`, `_outs`, `_log`, `_stdout`, `_stderr`). The shim
writes `_jobinfo` with the resolved threads/memory so the stage sees the same
allocation `mrp` would give it, tees the stage's `_log`/stdout/stderr to the
process's own streams (so logs survive on a cloud backend that discards the
scratch dir), and classifies failures: an `ASSERT`-class error exits **42**
(non-retryable), anything else exits 1 (retryable) — which the generated config
turns into Nextflow's terminate-vs-retry policy. Beside the adapter files it
writes `_mre_stage`, the name of the stage the phase ran. The adapter never
reads it; the `mrp` ABI differential (`TestMrpDiff`) uses it to group stage
invocations without depending on the Nextflow task graph, and the failure-parity
leg (`TestMrpDiffFailureParity`) to name the stage a failed task ran.

All three adapter ABIs — `py`, `exec`, and `comp` (via `mrjob`) — have been run
live in an isolated cloud container.

---

## 6. Bindspecs: how input wiring is described

Martian bindings ("this call's input `x` comes from that call's output `y`,
input `z` is the literal `5`") are compiled at transpile time into small JSON
**bindspecs**, one per call, under `_assets/bindspecs/`. Example:

```json
{ "values": { "ref": { "kind": "self", "id": "values", "output": "" } } }
```

This says the callee's `values` input is bound to the pipeline's own `values`
input (`kind: self`). Other kinds reference an upstream call's output
(`kind: call`, with `id` and `output`). A whole-call ref to a map call also
carries `fork` (`array` or `map`): the call's merged bundle holds one
collection per output, and the binder regroups it into one struct per fork,
which is the value mrp gives `x = CALL`. At runtime `mre bind` reads the bindspec,
the pipeline args, and the staged upstream bundles, and assembles the callee's
args bundle. Keeping the wiring as data (a bindspec) rather than baking it into
the Groovy means the binder — including typed-map field projections and
wildcard (`*`) expansion — is one well-tested Go code path, not regenerated
Groovy per call.

---

## 7. The bundle data plane

A **bundle** is the transpiler's neutral container for "a set of typed values,
including the files they reference." On disk it is a directory:

```
<bundle>/
  data.json        ← the typed JSON sidecar (scalars, arrays, maps, structs)
  f/L0000/reads.fastq  ← each file-typed leaf in its own ordinal dir (the
  f/L0001/reads.fastq    canonical type-walk order), so leaves never collide,
                         under the basename mrp passes the stage (#284);
                         data.json references them via a marker
  f/staticnull.mro2nf.json  ← optional: the top-level fields mrp resolves
                         to null when it builds the call graph (#436)
```

`mrp` resolves a call under an invocation-known true gate to `null` statically,
and prunes a map call over such a value to `null` where a runtime empty merges
to the typed empty. At run time a disabled call's bundle is `null` as a whole;
`mre bind` and `mre forkbind` list each param they bind to such a value (or to
a field another bundle lists) in `f/staticnull.mro2nf.json`, and `mre merge`
lists every output of a map call it prunes, so the fact crosses sub-pipeline
boundaries and cascades. The list sits under `f/` because a de-bundled channel
item carries only `data.json` and the `f/*` items.

The basename is the one the leaf's path had where `MarkFiles` found it: the
file a stage wrote (Martian's `<out>.<ext>` default, or any path the stage chose),
the entry input's own file, a split's chunk-def file, or the upstream leaf a
binding, fork or join forwarded unchanged. That is the path `mrp` passes on, so
a stage that reads a sample name or an extension from `os.path.basename()` sees
the same value under both runtimes.

Between processes, a bundle travels **de-bundled** (#13): the channel item is a
`tuple(path("X/data.json"), path("X/f/*", arity: '0..*'))` — the sidecar plus
each leaf's ordinal dir as its own `path` item — which Nextflow stages back into
one `X/` directory in the consuming task. Staging the ordinal dir rather than
the file keeps the ordinal the marker names (Nextflow restages an item under its
own name), and keeps a stage-chosen basename out of Nextflow's stage-in script,
which does not quote every shell metacharacter (§8). Leaves are individual
objects on an object-store work dir (staged per leaf, never re-materialized as a
copied bundle), and each input stages under its own directory so two inputs'
ordinal dirs cannot collide. (The keyed nested-map fork/merge subsystem still
stages whole bundle directories.)

In `data.json`, every file-typed leaf is replaced with a **marker** of the form
`@mre:file:f/L<nnnn>/<basename>` — a *bundle-relative* path. A leaf staged from a
**directory** (a `path` output, or a directory a stage wrote into a `file`-typed
out) uses `@mre:dir:` instead, so its ground-truth dir-ness — stat'd at staging
time — travels to publish rather than being inferred from the declared type.
Both prefixes resolve identically on read-back. The rules:

- **On write** (a stage produced an output that is a real file): the file is
  copied into the bundle's `f/` tree and the leaf is *relativized* to a marker.
  This is `shim.MarkFiles`, which uses the type table to find exactly which leaves
  are files (§3's `Param.IsFile`, recursing into structs/arrays/maps).
- **Escaping** (on write): every other string that begins with the reserved
  `@mre:` prefix (a string input, a `.mro` literal, a stage's string output, a
  file-typed out naming no file) is stored as `@mre:str:<original>`. A marker in
  `data.json` is therefore always one `MarkFiles` wrote at a file leaf the type
  walk found, never a user's string.
- **On read** (a stage is about to consume a bundle): markers are *absolutized*
  back to real paths inside the staged bundle, so the stage opens a real local
  file, and escaped strings get their original value back, so the stage sees
  exactly the string `mrp` would pass it. A marker is accepted only in the shape
  `MarkFiles` writes (`f/L<nnnn>/<basename>`: exactly one name under the
  ordinal dir, not `.` or `..`, bundle-local); any other `@mre:` string fails
  the read. Readers of a raw `data.json` restore the escapes too:
  `publish-layout` for the published outs, and `forkbind -elementfile` for the
  element the driver slices out of a sidecar.

Because the files travel *with* the bundle — declared as `path` outputs and
inputs — Nextflow stages them between tasks automatically, with no shared
filesystem required. This is what
makes cloud portability work: the same bundle runs on a laptop (shared FS), under
the Docker executor (isolated containers), and on AWS Batch with an S3 work dir.

Two details matter only on a real object store. Both are handled:

- **`.resolve()`, not string interpolation.** Head-node Groovy closures that read
  a bundle's `data.json` use `path.resolve('data.json').text`, never
  `file("${path}/data.json")`. The latter drops the `s3://` scheme; the former
  preserves it.
- **Symlink dereference.** Nextflow stages inputs as *symlinks*. When `mre` copies
  a file leaf into a bundle, it dereferences the symlink to the real target first
  — otherwise the bundle would carry a dangling link that breaks once staged into
  the next isolated container (`shim.CopyTree`). For a stage's own outputs the
  dereference is confined (`shim.Confinement`). An output, and every symlink it
  passes through, is classified as the stage's own file inside the task work dir
  (hard-linked into the bundle), a path in the work dir outside the stage's
  phase dir, i.e. a staged input (followed wherever it points, hard-linked), or
  a host path outside the task:
  - On the isolated container targets (`awsbatch`, `healthomics`, or any project
    transpiled with `-confine-outputs`, which emits `-confine` on every stage
    phase) such a path cannot be valid downstream, and copying it would upload
    host data, so the task fails, naming the output and the target.
  - On the `local` target, where `mrp` publishes a symlink and pipelines
    legitimately return paths into host reference dirs, the target is staged as
    a byte copy, never a hard link, so no later write through the bundle can
    alias the host file.

---

## 8. Pipeline inputs: run parameters and file staging

The top-level `call EP(reads = …, scale = …)` provides the pipeline's default
inputs. The emitter bakes those defaults into `entry_args/data.json` at transpile
time (resolving any relative file-default paths against the `.mro`'s directory so
the bundle is self-contained), and stages each file default into
`entry_args/f/`.

A file default is staged only when it resolves, after following symlinks,
inside the `.mro`'s directory or a `-mropath` root given on the command line
(the implicit default `.` is an `@include` search path only, not a root, so
transpiling from `$HOME` does not admit everything under it), and so does
every symlink staging would follow inside a directory default. Anything else
fails the transpile naming the input and the path, so a shared `.mro` whose
default points at `~/.ssh` cannot bake it into the project,
the HealthOmics `workflow.zip`, or a cluster copy. `-allow-external-files`
stages it anyway; use it for defaults you know are yours, such as the Cell
Ranger dry-run `.mro`'s absolute paths into the bundle's test data. A default
that does not exist is not staged and is not checked.

You can also override inputs **at launch** without re-transpiling:

- Every entry input is exposed as a nullable Nextflow parameter, `params.<name>`.
- At run start, a **`BUILD_ENTRY_ARGS`** process runs `mre entryargs`, which
  overlays the supplied values onto the baked defaults (a null value keeps the
  default). On AWS Batch you set them with `-params-file inputs.json`; on
  HealthOmics they are run parameters declared in `parameter-template.json`.
- A command-line `--<name> <value>` reaches `mre entryargs` as a string, so a
  scalar `int`, `float` or `bool` input is parsed strictly into its declared type
  (a base-10 int64; a JSON number; exactly `true`/`false`). A value that does not
  parse, or a string for an array, map or struct input, fails the run; set those
  with `-params-file`.
- Every float in a supplied value, typed or parsed and at any depth, is
  re-encoded the way `mrp` encodes a float literal (`'g'`, shortest form). That
  covers a `float` leaf of any shape and, inside an untyped `map`, any number
  written with a `.` or an exponent, so
  `--f 2.0` and a `-params-file` `{"f": 2.0}` both reach the stage as `2`, as
  `f = 2.0` does under `mrp`.
- The string guarantee holds only under the strict (v2) syntax parser, the
  default from Nextflow 26.04. The v1 parser (the default on earlier releases,
  or `NXF_SYNTAX_PARSER=v1`) converts CLI values before the script sees them:
  `--b True` becomes `true`, and `--s 007` becomes the integer `7`, which no
  later step can recover. On such a launch, set
  `NXF_DISABLE_PARAMS_TYPE_DETECTION=true`. `-params-file` and HealthOmics run
  parameters are unaffected.

**File-typed inputs (the Cell Ranger model).** A run value that is a file is an
`s3://` URI, which a worker cannot `stat` directly. So file-bearing inputs are
routed through Nextflow's own staging: the emitter flattens the input's file
leaves to a list, `file()`s them on the head node (Nextflow/HealthOmics localizes
each into the task), declares them as a `path` input to `BUILD_ENTRY_ARGS`, and
`mre entryargs` pops the staged paths back into the value and marks them into the
bundle. This works for **every shape** — a scalar `file`/`path`/directory, a
`file[]`, a `map<file>`, and a struct with file fields — using one
flatten-and-reconstruct mechanism whose ordering (arrays by index, maps by sorted
key, struct fields in declaration order) is shared between the Groovy flatten and
the Go reconstruction so they can't drift. An unset input keeps its baked
default. It, and a value with no file in it (`[]`, `{}`, a struct whose file
field is null), stage zero files: the `path` input takes `arity: '0..*'`, so no
placeholder file is needed. Each leaf goes through `file(x, glob: false)`, so
the head node never globs a name.

**Names Nextflow cannot stage.** Nextflow 26.04 writes each staged file into
the task's stage-in script as `ln -s <source> <name>`, escaping spaces, quotes,
`()`, `\`, `!`, `&`, `|`, `<>`, `` ` ``, `:` and `;` but not `*`, `?`, `[`,
`]`, `{`, `}`, `$` or control characters such as a tab. Bash then expands those
in the task: `s[1].txt` beside `s1.txt` stages `s1.txt` silently, `{a,b}`
splits into two words, `$HOME` is substituted. The emitter cannot quote that
script, so `Mro2nf.entryFile` rejects an override path holding any of these
characters, anywhere in the path or URI, before anything is staged; the error
names the input and the character. Rename such a file, or the directory
holding an override. A baked default is not affected: `mro2nf` copies it into
the entry bundle in Go, and a stage-in script only ever names the ordinal leaf
directory that holds it (§7).

**Which file values a run may supply.** `file()` on the head node fetches any
URL Nextflow has a provider for and reads any path the head node can see, and a
run parameter comes from whoever launches the run: a portal, a launcher
service, a shared HealthOmics workflow. So before `file()` sees a leaf,
`Mro2nf.entryFile` checks it against the target:

| Target | Accepted file value |
|---|---|
| `local` | a local path (relative, absolute, or `file:`), or an `s3://` URI for its `awsbatch` profile |
| `awsbatch`, `healthomics` | an `s3://` URI |

Anything else fails the run on the head node, before anything is fetched, with
an error naming the input and the scheme: `http(s)://`, `ftp://`, any other
scheme, a local path on a cloud target.
`--mro2nf_allow_remote_inputs true` (or `"mro2nf_allow_remote_inputs": true` in
a `-params-file`) admits any scheme, but never a local path on a cloud target.
Set it only when you trust whoever supplies the run parameters: the `mro2nf_`
prefix keeps it apart from the pipeline inputs, which still share the one
Nextflow param namespace until #303 separates them. The HealthOmics
`parameter-template.json` does not declare it, so a HealthOmics run cannot set
it. Baked defaults are not run parameters and are not checked.

Two leaves can share a basename — two FASTQs both named `reads.fastq` in
different sample directories, say. To keep them from clobbering each other when
Nextflow stages them into one task dir, the `path` input uses
`stageAs: '<name>_?/*'`, which puts each leaf in its own numbered subdir
(`<name>_1/reads.fastq`, `<name>_2/reads.fastq`, …) while keeping the original
filename. The staged paths are then handed to `mre entryargs` in that numbered
order, which matches the flatten order, so each leaf reconstructs to the right
slot regardless of name collisions.

All of these were verified live on AWS Batch + S3 and AWS HealthOmics (e.g. a
FASTQ-directory supplied as an `s3://…/dir/` prefix stages the whole directory
into the task).

---

## 9. `_assets`: shipping the type manifest and bindspecs to isolated workers

A process script can only read files that were staged into its (isolated) task
dir. Two transpile-time artifacts must reach every task: the **type manifest**
(`types.json` — needed for the file-leaf walk and scalar coercion) and a bind
process's own **bindspec**. Referencing them via `${projectDir}` works on a
shared filesystem but is invisible on AWS Batch + S3.

So each task stages exactly the asset files it needs as individual `path` inputs:
the shared `types.json`, plus — for a bind/fork process — only its **own**
`spec.json`. (An earlier design staged the whole `_assets` directory into every
task, which transferred every call's bindspec to every task; the per-bind form
cuts that to O(1) per task.)

---

## 10. Targets: local, AWS Batch + S3, AWS HealthOmics

`-target` shapes the config, the publish location, the container build, and the
packaging. The orchestration (`main.nf`, modules) is identical across targets;
only the surrounding plumbing changes.

| | **local** | **awsbatch** | **healthomics** |
|---|---|---|---|
| executor | `local`/HPC profiles | `awsbatch` (S3 work dir) | managed (no executor) |
| container | none (host tools) | required ECR image | required private-ECR image |
| data plane | shared FS | S3 object store | managed run filesystem |
| outputs | `results/` | S3 work dir; `--aws_outdir s3://…` to publish | `/mnt/workflow/pubdir` → S3 |
| packaging | — | `Dockerfile` + `.dockerignore` + `runtime/` build context | + `parameter-template.json` + `package.sh` (zip) |

For container targets the emitter writes a **`Dockerfile`** plus a self-contained
`runtime/` build context (the `mre` binary, the Martian adapters, your stage
code, and — for `comp` stages — the `mrjob` wrapper) at fixed in-container paths
(`/opt/mro2nf/…`) that the generated scripts bake. HealthOmics tasks have **no
internet**, so any third-party stage dependency (e.g. the Cell Ranger binary)
must be added to the Dockerfile. A file such a step COPYs (e.g. the Cell
Ranger tarball) must sit under `runtime/` or be un-ignored in `.dockerignore`;
a re-transpile regenerates both files and empties `runtime/`, so redo the
additions after one. The `.dockerignore` beside it admits only `runtime/` into
the build context and excludes the same junk names the vendoring skips
(below), so `docker build <project>` never uploads
`entry_args/`, run logs or a stray `.env` to the daemon.

HealthOmics' `package.sh` zips an allowlist, never the project dir: `main.nf`,
`nextflow.config`, `parameter-template.json`, `modules/`, `lib/`, `_assets/`,
`entry_args/` (`entry_resolved/` instead under `-native`), and `nulls/` when a
call can be disabled. It fails if one of them is missing, and anything else in
the directory — `.nextflow.log*`, params files, `work/`, `results/`, `.env` —
stays out of `workflow.zip`.

A stage `src` given as a path is vendored only when it resolves inside the
`.mro`'s directory or a `-mropath` root, as for entry file defaults (§8);
otherwise the transpile fails unless `-allow-external-files` is set. The local
target runs stage code in place and copies none, so it does not check. The
`-mre`, `-shell` and `-mrjob` tools passed as flags are the user's own and may
live anywhere; one a `.mro2nf.yml` sets must resolve inside those roots too
(with the `martian.py` beside a config-set `-shell`), since a config file
shipped beside a shared `.mro` is not trusted. Each is copied as the one file
named (plus the `martian.py` beside `martian_shell.py`), never with the rest of
its directory. `runtime/` is emptied before vendoring, so a re-transpile into
an existing project keeps nothing an earlier run put there.

A vendored stage directory leaves out VCS metadata (`.git`, `.hg`, `.svn`,
`.bzr`), `.env*` files, `__pycache__`/`*.pyc`/`*.pyo`, and editor and OS
litter. A symlink in it stays a relative symlink when its target is vendored
too; one that leaves the directory, dangles, or points at a left-out entry
fails the transpile, since following it would ship a file from beyond the
tree. Every copied file keeps its own permission bits (a linked `src` its
target's), never the link's.

Two cloud-specific defaults to know about:

- **`awsbatch` publishes nothing by default.** A transpiled stage's outputs are
  already uploaded to the S3 work dir by the Batch executor — that *is* the
  canonical output. `params.outdir` defaults to null (no launcher-local copy);
  pass `--aws_outdir s3://…` to also copy the curated final outputs to a stable
  S3 location.
- **The CDK example deploys cost-safe infra.** `deploy/awsbatch-cdk/` provisions a
  minimal Batch + S3 + ECR setup that scales to zero between runs (no idle cost),
  on spot, with no NAT and S3-lifecycle expiry.

---

## 11. Faithful vs. divergent, and how it's validated

The transpiler aims for **byte-identical outputs to `mrp`**, and every supported
feature is checked against the real `mrp` output. The validation tiers:

- **Unit tests** — the IR, binder, type walk, shim ABI, and the generated
  Nextflow text.
- **Local e2e** (`make test-e2e`) — the golden fixture table transpiled, run
  under Nextflow, and diffed against committed `mrp` goldens (plus
  `TestCloudSim*`, the object-store data plane under copy-staging).
- **Generative differential** (`TestGenDiff`, `make gen-diff`) — random
  well-typed programs (`test/gen`) whose stub stages derive every output from
  a hash of their inputs, run under live `mrp` and under mro2nf; any
  difference in the published outputs, a stage's `_args`/`_outs` or the run
  outcome is shrunk to a minimal fixture. Every comparator rule of the
  divergence allowlist, issue-tracked ones included, excuses a difference
  there, though none is reported as excusing nothing; the ABI and outcome
  matchers, which name their fixtures, do not apply. `make test-e2e` runs a
  fixed smoke set of seeds; `make gen-diff` runs a seed range for the long
  tier, which the weekly nightly tier starts from a random seed and ends after
  `GEN_BUDGET` minutes.
- **Container isolation** (`make test-e2e-docker`) — the same pipelines under the
  Docker executor, where each task mounts only its work dir. This is the
  license-free proxy for the cloud "no shared filesystem" model and the regression
  guard for the bundle/staging code.
- **Live AWS** — the fixture set run end-to-end on real AWS Batch + S3 and AWS
  HealthOmics, byte-identical to `mrp`, exercising all three adapters
  (`py`/`exec`/`comp`), file inputs/outputs through the object store, directory
  and `file[]`/`map<file>`/struct file inputs from S3, data-dependent splits,
  and stage logs. See [`LIVE_AWS_TEST.md`](LIVE_AWS_TEST.md).

The known **divergences** are the allowlist `test/e2e/divergences.json`, which
the differential applies and which generates the
[divergence table of `FEATURE_COVERAGE.md`](FEATURE_COVERAGE.md#divergences-from-mrp).
Some change only how the outputs are reached: `local`, `volatile` and `retain`
are no-ops (no VDR / mid-run work-dir reclamation), the published `outs/` tree
is copied into place rather than moved and symlinked, `-resume` is Nextflow's
content-addressed cache, and a mapped or disabled `preflight` runs in plain DAG
order. Others are real output or outcome differences, each with its tracking
issue or the reason it is kept.
The transpiler is **loud** about anything it cannot lower faithfully —
unsupported constructs are hard transpile errors, and documented behavioral
no-ops are logged as warnings at transpile time.
