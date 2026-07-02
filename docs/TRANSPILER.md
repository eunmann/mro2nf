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
set of stages, pipelines, calls, and bindings.

The `mro2nf` CLI (`cmd/mro2nf/main.go`) ties this together:

- Flags: `-o <dir>` (output project), `-mre`/`-shell`/`-mrjob` (paths to the
  runtime shim, the Martian Python adapter, and the optional compiled-stage
  wrapper, all baked into the generated scripts), `-mropath` (search path for
  `@include`), `-target` (`local` | `awsbatch` | `healthomics`), `-container`
  (image URI for cloud targets), `-monitor` (per-stage virtual-memory
  enforcement).
- It resolves the stage source paths and the runtime tool paths to **absolute**
  paths (so the generated project runs from any working directory), records the
  `.mro`'s directory as `MRODir` (used to resolve relative file defaults — see
  §8), and calls `emit.Emit(prog, opts)`.

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
_assets/
  types.json                         ← the program's type manifest
  bindspecs/BIND_*.json              ← one binding spec per call (see §6)
entry_args/data.json                 ← the baked top-level call arguments (§8)
```

### 4.1 A plain stage → a process

A non-split stage becomes a single Nextflow process whose `script` block runs
`mre main`, wrapped in a `wf_<stage>` subworkflow:

```groovy
process SOMESTAGE {
  cpus 1
  memory '1 GB'
  input:
    path args                  // the stage's input bundle (a directory)
    path 'types.json'          // the shared type manifest (staged; see §9)
  output:
    path "outs__${args.baseName}", type: 'dir'   // the output bundle
  script:
    """
    'mre' main … -args ${args} -outs 'y' … -o outs__${args.baseName}
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

The process takes a **bundle directory** as input and emits a **bundle
directory** as output. Bundles are the universal currency between processes
(§7).

### 4.2 A split stage → SPLIT / MAIN / JOIN

Martian's `split` runs a stage in three phases — split the work into chunks, run
each chunk, join the results. The emitter renders three processes and wires them
in `wf_<stage>`:

```groovy
process SUM_SQUARES_SPLIT { … script: "mre split … -o chunks.json -joinres joinres.json -chunkdir ." }
process SUM_SQUARES_MAIN  { … script: "mre main  … -chunk ${chunk} -o out_${chunk.baseName}" }
process SUM_SQUARES_JOIN  { … script: "mre join  … -chunkdefs ${defs} -chunkouts … -o outs" }

workflow wf_SUM_SQUARES {
  take: args
  main:
    types = file("${projectDir}/_assets/types.json")
    SUM_SQUARES_SPLIT(args, types)
    // each chunk dir carries its own resource request in data.json; read it
    // with .resolve() (object-store-safe) and tuple it with the chunk:
    chunks = SUM_SQUARES_SPLIT.out.chunks.flatten().map { f ->
        tuple(new groovy.json.JsonSlurper().parseText(f.resolve('data.json').text).resources, f) }
    SUM_SQUARES_MAIN(chunks.combine(args), types)
    join = SUM_SQUARES_SPLIT.out.joinres.map { f -> new groovy.json.JsonSlurper().parseText(f.text) }
    SUM_SQUARES_JOIN(join, args, SUM_SQUARES_SPLIT.out.defs, SUM_SQUARES_MAIN.out.collect(), types)
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
- **Split-returned join override.** A `split()` may also return a `join`
  resource request; SPLIT writes it to `joinres.json`, the workflow parses it
  into a `join` value, and JOIN's `cpus`/`memory` read it — so the join phase is
  scheduled like `mrp` would.

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
  returns) forward one upstream call's entire output bundle verbatim, no
  process is emitted at all: the producer's output channel is routed straight
  to the consumer.

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
  single array/map-shaped output bundle.

Every stage and pipeline therefore gets **two** emitted workflows: the plain
`wf_<name>` and the keyed `wf_<name>_map`. The keyed forms exist so a map call
over a split stage (or over a sub-pipeline that itself maps — *nested* maps) keys
every chunk and join by its fork. Fork directories are enumerated by **reading
the staged `forknames.json`** (via `.resolve()`), never `java.io.File.listFiles()`
— the latter cannot list an S3 work dir.

### 4.5 Disabled calls → a runtime gate

A `call … using (disabled = …)` is resolved at runtime: a **DISABLE process**
(`mre bind … -o disable`) computes the boolean, and the workflow `branch`es on it
— running the callee only for the enabled case and emitting a pre-baked
*null-outputs* bundle (from `nulls/<call>`) for the skipped case. Disabled map
calls gate per fork.

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
  stage's final output bundle.
- **`mre bind` / `mre forkbind`** — resolve a call's input *bindings* (from a
  bindspec + the staged upstream bundles + pipeline args) into the callee's args
  bundle. `forkbind` produces one bundle per fork.
- **`mre merge`** — gather per-fork output bundles into one array/map output.
- **`mre entryargs`** — build the top-level args bundle from the baked defaults
  overlaid with launch-time run parameters (§8).
- **`mre publish-layout`** — compute the mrp-style `outs/` layout from the final
  sidecar alone (no file leaves staged), writing `layout.json`,
  `pipeline_outs.json`, and `manifest.json.gz` for the PUBLISH_LEAF fan-out
  (§4.7). (`mre publish` is the older copy-everything form of the same walk.)

Under the hood each phase hands the stage to the **real Martian adapter** —
`martian_shell.py` for `py` stages, an exec wrapper for `exec` stages, and the
`mrjob` wrapper for compiled (`comp`) stages — using the same on-disk ABI Martian
uses (`_args`, `_jobinfo`, `_outs`, `_log`, `_stdout`, `_stderr`). The shim
writes `_jobinfo` with the resolved threads/memory so the stage sees the same
allocation `mrp` would give it, tees the stage's `_log`/stdout/stderr to the
process's own streams (so logs survive on a cloud backend that discards the
scratch dir), and classifies failures: an `ASSERT`-class error exits **42**
(non-retryable), anything else exits 1 (retryable) — which the generated config
turns into Nextflow's terminate-vs-retry policy.

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
(`kind: call`, with `id` and `output`). At runtime `mre bind` reads the bindspec,
the pipeline args, and the staged upstream bundles, and assembles the callee's
args bundle. Keeping the wiring as data (a bindspec) rather than baking it into
the Groovy means the binder — including typed-map field projections and
wildcard (`*`) expansion — is one well-tested Go code path, not regenerated
Groovy per call.

---

## 7. The bundle data plane

A **bundle** is the transpiler's neutral container for "a set of typed values,
including the files they reference." Every channel item between processes is a
bundle *directory*:

```
<bundle>/
  data.json        ← the JSON payload (scalars, arrays, maps, structs)
  f/0/<basename>   ← each file-typed leaf, copied in and numbered so basenames
  f/1/<basename>     never collide; data.json references them via a marker
```

In `data.json`, every file-typed leaf is replaced with a **marker** of the form
`@mre:file:f/<n>/<basename>` — a *bundle-relative* path. The rules:

- **On write** (a stage produced an output that is a real file): the file is
  copied into the bundle's `f/` tree and the leaf is *relativized* to a marker.
  This is `shim.MarkFiles`, which uses the type table to find exactly which leaves
  are files (§3's `Param.IsFile`, recursing into structs/arrays/maps).
- **On read** (a stage is about to consume a bundle): markers are *absolutized*
  back to real paths inside the staged bundle, so the stage opens a real local
  file.

Because the files travel *inside* the bundle directory, Nextflow stages them
between tasks automatically, with no shared filesystem required. This is what
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
  the next isolated container (`shim.CopyTree`).

---

## 8. Pipeline inputs: run parameters and file staging

The top-level `call EP(reads = …, scale = …)` provides the pipeline's default
inputs. The emitter bakes those defaults into `entry_args/data.json` at transpile
time (resolving any relative file-default paths against the `.mro`'s directory so
the bundle is self-contained). But you can also override inputs **at launch**
without re-transpiling:

- Every entry input is exposed as a nullable Nextflow parameter, `params.<name>`.
- At run start, a **`BUILD_ENTRY_ARGS`** process runs `mre entryargs`, which
  overlays the supplied values onto the baked defaults (a null value keeps the
  default). On AWS Batch you set them with `-params-file inputs.json`; on
  HealthOmics they are run parameters declared in `parameter-template.json`.

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
the Go reconstruction so they can't drift. An unset input keeps its baked default.

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
| packaging | — | `Dockerfile` + `runtime/` build context | + `parameter-template.json` + `package.sh` (zip) |

For container targets the emitter writes a **`Dockerfile`** plus a self-contained
`runtime/` build context (the `mre` binary, the Martian adapters, your stage
code, and — for `comp` stages — the `mrjob` wrapper) at fixed in-container paths
(`/opt/mro2nf/…`) that the generated scripts bake. HealthOmics tasks have **no
internet**, so any third-party stage dependency (e.g. the Cell Ranger binary)
must be added to the Dockerfile.

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
- **Local e2e** (`make test-e2e`) — 62 cases over 57 `.mro` fixtures transpiled,
  run under Nextflow, and diffed against committed `mrp` goldens (plus
  `TestCloudSim*`, the object-store data plane under copy-staging).
- **Container isolation** (`make test-e2e-docker`) — the same pipelines under the
  Docker executor, where each task mounts only its work dir. This is the
  license-free proxy for the cloud "no shared filesystem" model and the regression
  guard for the bundle/staging code.
- **Live AWS** — the fixture set run end-to-end on real AWS Batch + S3 and AWS
  HealthOmics, byte-identical to `mrp`, exercising all three adapters
  (`py`/`exec`/`comp`), file inputs/outputs through the object store, directory
  and `file[]`/`map<file>`/struct file inputs from S3, data-dependent splits,
  and stage logs. See [`LIVE_AWS_TEST.md`](LIVE_AWS_TEST.md).

The known **divergences** are documented in [`FEATURE_COVERAGE.md`](FEATURE_COVERAGE.md)
and are all behavior-only (never output-affecting): a `preflight` bound to
pipeline inputs gates the rest of the pipeline (mrp's early abort), but one
bound to a call output runs in plain DAG order; `local`/`volatile` are no-ops
(no VDR / mid-run work-dir reclamation); and the published `outs/` tree is
copied into place rather than mrp's move+symlink (same layout and contents).
The transpiler is **loud** about anything it cannot lower faithfully —
unsupported constructs are hard transpile errors, and documented behavioral
no-ops are logged as warnings at transpile time.
