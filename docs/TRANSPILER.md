# How the Martian → Nextflow transpiler works

This document explains, end to end, how `mro2nf` turns a Martian
pipeline (`.mro`) into a runnable Nextflow project — what each piece does, how
data flows, and why the design is shaped the way it is. It is meant to be read
top to bottom by someone who knows neither codebase deeply.

If you want the per-feature fidelity table (what is faithful vs. divergent), see
[`FEATURE_COVERAGE.md`](FEATURE_COVERAGE.md).

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
**not** rewrite or reimplement your stage code. Every stage job runs the
**original, unmodified** Martian stage through `mrjob` and the Martian adapter,
started by a small Go shim called **`mre`** exactly as `mrp` starts it. The
stage can't tell the difference.

This one decision shapes everything else:

- **Correctness.** Martian itself resolves what a stage receives: `mre`
  compiles the run's invocation with Martian's own compiler, walks Martian's own
  call graph, and resolves each stage's `_args` with Martian's own binding and
  type functions. The stage runs through the real adapter, so its outputs are
  what `mrp` would produce. The transpiler never re-derives the science.
- **Two programs.** `mro2nf` is the transpiler; it runs at build time and emits
  the Nextflow project. `mre` is the runtime shim, which every task calls at
  execution time.
- **Nextflow owns the orchestration.** One process per stage node of Martian's
  call graph, wired by the data the nodes exchange: Nextflow channels carry each
  instance's fork, its gates and the outputs it reads, and native `path` staging
  moves the files.

```
          build time (mro2nf)                       run time (Nextflow + mre)
   ┌──────────────────────────┐           ┌─────────────────────────────────────┐
   │  pipeline.mro            │           │  PLAN   mre plan: compile the run's │
   │      │  compile (Martian) │  emits    │         invocation, fix its forks   │
   │      ▼                    │  ──────►  │  N0_X   mre args → mre run → post   │
   │  witness call graph      │           │  N1_Y   … one process per stage node│
   │      │  topology          │           │  OUTS   mre outs: mrp's PostProcess │
   │      ▼                    │           │  each mre run drives the real stage │
   │  main.nf + stage.nf      │           │  through mrjob and the adapter      │
   └──────────────────────────┘           └─────────────────────────────────────┘
```

---

## 2. The pipeline at a glance

```
.mro file
   │  cmd/mro2nf (the `mro2nf` CLI)
   ▼
cg.ReadSource     → the entry file's declarations, its call, and the call's
   │                invocation data (compiled as mrp compiles it)
cg.Source.Witness → the call graph of a witness invocation that holds every
   │                stage node any invocation of the call can run
cg.Graph.Topology → per node its fork roots, reads and gates
   │
cgnf.Emit         → a Nextflow project on disk:
   │                  main.nf, stage.nf, nextflow.config,
   │                  _assets/mro/, _assets/types.json
   ▼
nextflow run main.nf …  →  PLAN, a task per stage instance, then OUTS
```

Source layout:

```
cmd/mro2nf/      → transpiler CLI: .mro -> Nextflow project
cmd/mre/         → runtime shim: plan | args | run | post | outs
internal/
  cg/            → Martian's compiled call graph: witness, topology, plan,
                   the _args resolver, fork elements, gates, pipeline outs
  cgnf/          → call graph -> Nextflow project (main.nf wiring, stage.nf)
  job/           → runs one stage job through mrjob as mrp runs it
  frontend/      → lowers a Martian AST to the typed parameters of the manifest
  ir/            → typed parameters of the program's callables and structs
  types/         → type-directed file-leaf walk + type manifest
  shim/          → bundle I/O, markers, output confinement
  config/        → .mro2nf.yml project defaults for the transpiler flags
  logging/       → log/slog setup (stderr)
```

---

## 3. Transpile: the witness call graph

The transpiler does not re-implement the Martian grammar. `cg.ReadSource`
compiles the entry `.mro` with the upstream
`github.com/martian-lang/martian/syntax` package and builds its invocation data
(`core.BuildDataForAst`), Martian's JSON form of an invocation. Compiling the
call graph (`MakePipelineCallGraph`) is the step `mrp` runs before it creates a
pipestance, so a pipeline `mrp` refuses to start is refused at transpile time
with `mrp`'s message, and no project is written.

**The CLI.** `mro2nf` (`cmd/mro2nf/main.go`) takes:

- `-o <dir>`: the output project.
- `-mropath <path>`: the search path for `@include` and stage `src`; default
  `$MROPATH` when set, else the `.mro`'s directory, as `mrp`'s
  `cmd/mrp/configure.go` computes it, and never the working directory. As in
  `mrp`, the entry's own directory is searched first for the entry's includes
  and its stages' code only: every compile of the run's invocation, in the
  transpiler and in each task, compiles it as the entry file.
- `-mre`, `-shell`, `-mrjob`: the tools every task runs, baked into
  `nextflow.config`. `-shell` names Martian's python adapter
  (`<adapters>/python/martian_shell.py`), and the project takes Martian's
  adapters dir from it; it is required. `-mrjob` defaults to the `mrjob` beside
  `-mre`. A relative path is made absolute, against the working directory for a
  flag and against the config file's directory for a `.mro2nf.yml` key. A bare
  `-mre` or `-mrjob` name stays a `PATH` command.
- `-monitor`: mrp's `--monitor`; `mrjob` enforces each job's memory and thread
  limits.
- `-confine-outputs`: fail a stage task whose output resolves outside its work
  dir instead of copying it in (§7), for a project run under an isolating
  executor such as docker.
- `-config <path>`: a `.mro2nf.yml` of per-project defaults (`mre`, `shell`,
  `mrjob`, `monitor`); an explicit flag always wins.
- `-version`.

**The witness.** A transpile cannot know a run's invocation, so it compiles a
*witness* one (`cg.Source.Witness`): every entry input a value of its type with
two elements in every collection, nested collections of different lengths,
false for every bool and no null anywhere. No map call is pruned on an empty or
null source and no call is disabled by an entry value, so its call graph holds
every stage node, with its fork roots, of any invocation. Where `mrp` refuses
the witness (it refuses a runtime-gated split of a non-empty literal), each
input takes its witness value over the literal invocation's only while `mrp`
still accepts the call. `TestWitnessCoversLiteral` checks every fixture's
literal invocation is within its witness's.

**The topology.** `cg.Topology` reduces the witness call graph to what tasks
exchange: per node its fork roots, the outputs it reads of other nodes, and the
nodes whose outs gate it; per fork root what feeds its forks (the entry, or one
stage) and the outer roots its forks vary with. A disable binding forked on a
map call its gating stage feeds (a split of its outs, as in
`map call X(item = split SRC.items)` under `disabled = self.item.skip`) has a
value per fork of that call; the gating stage's post resolves it at each and
posts the forks it disables (`split_gate`), and so it does when the entry forks
that call (`split_gate_entry`). A shape whose data the tasks cannot exchange — a
fork root several stages feed, a disable binding that reads several stages, or
one forked on a map call another stage feeds — is refused at transpile time.

---

## 4. The generated project

```
main.nf               ← PLAN, one process call per stage node, then OUTS: wiring only
stage.nf              ← the one hand-written stage module every node includes
nextflow.config       ← params, retry policy, GPU requests, trace, executor profiles
_assets/mro/          ← a copy of every .mro file the entry compiles from, each
                        at its absolute path below it; PLAN compiles it
_assets/types.json    ← the type manifest the OUTS task lays the outs out with
```

**`stage.nf`** declares `STAGE` (a non-split stage) and `STAGE_SPLIT`,
`STAGE_MAIN` and `STAGE_JOIN` (a split stage's three phases), plus `OUTS`.
`main.nf` includes them once per node under its own alias
(`include { STAGE as N3_DBL }`, a split stage as `N1_SUM_SQUARES_SP`, `_MN`
and `_JN`), so a `withName:` selector names the node. Every stage task runs
`mre args`, then its job through `mre run`, then `mre post`. The driver runs a
handful of named Groovy helpers (`ordered`, `jsonOf`, `sized`), which read only
records mre wrote; it parses no stage-controlled JSON.

**`nextflow.config`** holds:

- `params.outdir` (`results`), and `params.job_resources`, Martian's
  `MRO_JOBRESOURCES` analog: a map from a stage's `special` key to the
  `clusterOptions` of a grid executor.
- `params.mro2nf_mre`, `mro2nf_mrjob`, `mro2nf_adapters`, `mro2nf_psid`,
  `mro2nf_monitor` and `mro2nf_confine`: the tools and knobs the stage module
  reads.
- One `params.<name> = null` per entry input (§8).
- The retry policy, `mrp`'s: a task is retried, up to twice, only on exit 75,
  the failures `mrp` retries (§6), or on a signal exit of its own (137, 143),
  such as the executor's out-of-memory kill. An ASSERT (42), a stage exception
  or a failed `mre` step ends the run on its first attempt. A retry gets the
  stage's memory times the attempt, except under `-monitor`, where `mrjob`
  kills the stage at its own request as `mrp` does and `mrp` does not retry
  that kill.
- One `accelerator` rule, evaluated per task, that requests N GPUs for a job
  whose special, as `mrp` resolves it, is `"gpu"` or `"gpu:N"`: the stage's own,
  or for a chunk or the join the `__special` the split returned for it (see
  [`GPU.md`](GPU.md)).
- Task tracing, and the executor profiles: `standard` (local, clamped to the
  host's cpus and memory with `process.resourceLimits`), `slurm`, `sge`,
  `lsf` and `pbs`.

---

## 5. Run time

**PLAN.** One task starts every run. It stages the project's copy of the
entry's `.mro` files (`_assets/mro/`) and compiles that. `mre plan` overlays
the run's entry params on the entry's invocation data (a string param of a
scalar input parsed as its type, §8), rebuilds the call as `mrp`'s invocation
generator does (`core.BuildCallAst`), and compiles it over the entry's
declarations, so `mrp`'s refusals fail the run before any stage starts. It
fails if the call graph has a node, a fork root or a read the witness lacks
(`Topology.Covers`); a node the invocation disables outright counts only by
its presence. Its records reach the driver as `eval` values: the run's call;
per node a digest of its resolved spec (stage, bindings, disable bindings and
fork roots in Martian's JSON form), whether the invocation disables it, and the
fork keys a static per-fork binding disables; per entry-fed root its forks,
keyed by the witness's outer roots, each with its fork record and its index
token.

**Cache identity.** PLAN caches on its inputs: the entry values, the `.mro`
copy and its meta. A task's `-resume` key holds its own node's digest (in
`meta`), the run's call, the mro2nf build, and its fork records and staged
dirs. No task stages a file of PLAN's: the call and the digests reach it as
values, which Nextflow keys on by content, so a re-transpile whose `.mro` edit
leaves a node's spec and the call as they were re-runs PLAN but keeps the node
cached (`TestResumeKeysOnlyOnATasksOwnTypes`). A changed entry value changes
the call, and so re-runs every task.

**Code.** A stage task compiles the run's call over the entry `.mro` where the
run finds its code: its includes and its stages' code resolve beside it, as
under `mrp`. `mre args` fails the task when the node compiles to another spec
than the digest PLAN planned (`cg.ErrSpecDrift`): a `.mro` edited since the
transpile, or an image built from other sources, fails loudly rather than run.

**Instances.** An instance's key is the ordinal of its fork of each of its
roots. The driver enumerates a node's instances root by root: an entry-fed
root's forks come from PLAN, a stage-fed root's from its producer's
`forks/<root>/<ordinal>/` output, and each instance joins the forks agreeing on
the outer roots. An instance zipping another stage's output over a root joins
that stage's partner dir at its own fork's token, or the absent record, so each
fork stages one element of it whatever the fork width (the `zip` bench lanes
gate the bytes one task stages flat in N).

**Preflights.** As in `mrp`, each instance waits until every instance of each
preflight it is gated by is done: a pipeline's preflights gate every other call
in it, and through a sub-pipeline call every call within, its preflights too
(`preflight_order`). A disabled preflight instance counts as done, and a failing
one ends the run before the calls it gates start (`preflight_fail`).

**Gates.** A node's gates are decided on the driver: PLAN's record and each
gating stage's posted `gates` (an `eval` output), joined on the roots they
share. A disabled instance runs no task and stands in with no outs and no
forks; Martian's call graph already reads its outputs as null in every
consumer.

**Reads.** An enabled instance stages, of each node it reads, only the outputs
it reads (`outs/<output>/`): from the instance that shares its roots, or, a
gather, from every such instance under it. A forked instance's gather over one
inner map call is a `groupKey` of its share's fork count, which PLAN writes for
an entry-fed root and the producer's posted forks give for a stage-fed one, an
empty share gathering at once, so each instance starts when its own share is
done (`mrp` itself waits for every fork of a node it reads;
`TestEmitSizesForkedGathers`). Any other gather is a seeded `groupTuple` over all
of its upstream, which an unforked consumer needs anyway.

**Split stages.** A split's chunk outs are gathered by `groupKey` over the
chunk count. Each chunk's main and the join are scheduled with the cpus, memory
and special their jobs get, the split's `resources.tsv`, an `eval` value the
driver splits into rows without parsing a record.

**OUTS.** The last task stages every outs bundle the pipeline outs read, gets
each bundle's location as a value (`toUriString()`, a path or an `s3://` URI),
and publishes them with `mre outs` through `mrp`'s own post-processing: the
`outs/` tree and `pipeline_outs.json`, which `publishDir` copies into
`params.outdir`. The workflow `output {}` block would declare that tree as
data, but Nextflow 24.04, the engine floor, accepts it only under the
`nextflow.preview.output` flag, in a `publish:` syntax (`ch >> 'name'`) that
26.04 rejects, and 26.04's syntax does not compile on 24.04 (#356).

**Order.** Each staged list is staged in an order the run fixes, a gather's and
OUTS's by instance key and a join's by chunk name, since list order is part of
a task's `-resume` key and arrival order is completion order.

**Overhead.** The tasks are PLAN, one per stage job `mrp` runs (each split,
chunk and join of a split stage, one per fork of a non-split stage), and OUTS.
Binding, forking, gating and gathering run inside those tasks or on the driver,
so the plumbing task count does not grow with fork width or chunk count
(`make bench`, `make test-scale`).

---

## 6. The runtime shim `mre`

Each subcommand that works on the call graph takes the entry `.mro`
(`-entry`, `-mropath`) and the run's call (`-call`, base64 of the call PLAN
rebuilt; the entry's own call without it), compiles the call over the entry's
declarations as `mrp` compiles an invocation (`ParseSourceBytes`, then
`MakePipelineCallGraph`) as the entry file, and reads only the items its task
staged (`-in`, an item or a dir of items). Without `-types`, each builds the
type manifest from the invocation's own declarations.

- **`mre plan`** starts a run (§5). It rebuilds the run's invocation from the
  transpiled entry and the run's entry values, compiles it, checks it against
  the witness, vets the entry file values a task stages (§8), and writes the
  call every task compiles, one record per witness node, the entry files of
  each node's instances, and the forks of each entry-fed fork root.
- **`mre args -node <fqid> -digest <d>`** builds one stage instance's args
  bundle and prints `enabled` or `disabled`, once the node compiles to the spec
  digest PLAN planned. The instance's fork comes from the fork elements it
  staged, or the fork records it got as values (`-fork`, base64 of a
  `fork.json`), and its values from its parents' stamped outs bundles. A
  disabled instance gets only its stamp, `instance.json`, which records the fork
  and each part's fork count, from which `mrp`'s fork id follows. A map call
  that zips several splits forks over its master source: a split of known
  length if one has it, else its first split in source order. The other splits
  resolve at the same index, and, as in `mrp`, read null past the end of a
  shorter source or from a null one, and drop a longer one's extra elements.
  The missing element is logged, as at `mrp`'s default enforcement level;
  `mrp --strict` is not supported.
- **`mre run -phase split|main|join`** runs one phase of the instance that
  `-args` stamps as `mrp` runs its job (`internal/job`). py and comp stages run
  under `mrjob`, built from the pinned module (`make build`), with `mrp`'s argv:
  `mrjob <adapters>/python/martian_shell.py <code> <phase> <meta>
  <meta>/files <journal>`. Exec stages run directly. Before the job, mre writes
  what `mrp` writes, through Martian's own types and metadata writer: `_args`
  (the chunk or join def's resolved resources over the bindings), `_jobinfo`,
  the pre-populated `_outs`, `_chunk_defs` and `_chunk_outs` for a join, and the
  files, tmp and journal dirs. `_jobinfo` carries `mrp`'s name,
  `ID.<psid>.<fqid>.<fork id>[.chnkN]`, the top-level call's invocation, the
  Martian runtime's and the pipelines' versions, and `monitor_flag` under
  `-monitor`, which `mrjob` enforces. `mrjob` exits 0 however the stage ends,
  so mre reads the outcome from the metadata: `_assert` exits 42, which is
  never retried, and `_errors` or a non-zero exit exits 1, or 75 when a line of
  the error matches a `retry_on` pattern of `mrp`'s `jobmanagers/retry.json` (a
  signal, ENOMEM, a scheduler failure). A split writes `-o` as one dir per
  chunk, named as `mrp` names it (`chnkN`), with its args bundle and
  `chunk.json`, plus `join.json` and `resources.tsv`: per chunk, then the join,
  its name, cpus, memory and special, as its job's `_jobinfo` records them
  (`job.Resources`, Martian's resolution of the stage's `using` under the def
  the split returned). A main or join writes its outs bundle. The
  files a job names in either are confined to the work dir (§7), strict under
  `-confine`. Beside the job's metadata mre writes `_mre_stage`, the name of
  the stage the job ran. The adapter never reads it; the `mrp` ABI differential
  (`TestMrpDiff`) uses it to group stage invocations without depending on the
  Nextflow task graph, and the failure-parity leg (`TestMrpDiffFailureParity`)
  to name the stage a failed task ran.
- **`mre post`** runs after the stage. It stamps the outs bundle with the
  instance (`instance.json`). For a disabled instance it first writes mrp's
  disabled outs, with every output null. It checks enabled outs with Martian's
  `LazyArgumentMap.ValidateOutputs`, as mrp's `Fork.verifyOutput` does. Then it
  writes one dir per fork of every root this stage feeds:
  `forks/<root fqid>/<ordinal>/`. The ordinal is zero-padded, in mrp's fork
  order (array index, or map keys in byte order). Each dir holds that fork's
  element of the source collection as a bundle, with its file leaves staged,
  and its element of every other output of this stage a split over the root
  reads (a map call zipping two outputs, a disable binding splitting one), plus
  `fork.json`, which records the real fork (keys as JSON strings), its position,
  and the zipped outputs too short to have an element there, which read null as
  in `mrp`. A map key's fork dir also carries the key's token, the hex SHA-256
  of the key, after an `@` (an array fork's token is its index). For each
  output of this stage that a map call it does not fork zips (a split over
  another stage's or a literal's root), it writes one partner dir per element,
  `forks/<root>@<node>.<output>/<token>/`, and an absent record, `none`, which
  reads null where the output is null and a missing element, logged, at any
  index it lacks, as `mrp` reads it; a zip of maps at run time need not share
  keys (`zip_runtime_map`). An instance therefore stages only its own elements,
  and no key ever needs escaping. `mre post -entry-forks` writes the forks the call graph already
  fixes (literal and entry-sourced sources) the same way, as fork records
  without values. A root whose fork set varies with an outer map call (its
  source, of unknown length, is forked on that call) gets its forks under each
  fork of that call. The posting task enumerates an outer call its own fork does
  not fix from what it staged, and prefixes the name with the ordinal of each
  such call, joined by dots. A root of known length is keyed by its own forks
  alone, whatever outer calls its stages are also forked on. With `-fields`, it
  also writes the outs as one stamped bundle per output, `<dir>/<output>/`, so a
  consumer stages only the outputs it reads. With `-gates`, it writes the
  instances the instance's outs disable: of each node with a disable binding
  that reads this stage alone, every fork it resolves true at, as `mrp`'s
  `Fork.disabled` reads it (a null reads false), named
  `<node>@<roots>@<ordinals>` by the node's ordinals of the roots, beyond this
  stage's, the binding is forked on. Each bundle it posts also records, in
  `task.json`, the dir it posted from, where its stage ran, and the bundle's
  path there.
- **`mre outs`** resolves the pipeline outs, checks them with
  `MarshalerMap.ValidatePipelineOutputs`, and publishes them with `mrp`'s own
  post-processing (`Pipestance.PostProcess`), so the published tree and the
  outs JSON follow `mrp`'s rules by construction. It reconstructs a pipestance
  (`-work`): the top fork's `_outs` holds the resolved outs, each file leaf
  placed by its origin (§7). A stage's own file is one path in the pipestance
  however many outputs reach it, which PostProcess moves into `outs/` at the
  first and links the others to; a linked file is a path outside the
  pipestance per output that reaches it, which PostProcess links into `outs/`
  at that output. The pipestance is attached read-only from the invocation,
  with a runtime that needs no job manager, so no Martian install is read.
  PostProcess reports no error, so `mre outs` then checks every name it wrote:
  a path in `outs/` must exist, and no path of the reconstruction may reach the
  outs JSON. It writes `outs/` with each link replaced by what it reaches
  (`-o`) and the outs JSON (`-json`), naming a published file by its path in
  the tree, a linked one by its own link's path, and a file left unpublished
  (below an array of two or more dimensions) by where its producer's work dir
  keeps the copy it staged: `-locations` maps each staged bundle to that
  location, which no task sees of itself. A path the stage declared and never
  wrote is named below the work dir of the task that posted it (`task.json`),
  which a scratch dir is not.

The resolver mirrors only what Martian does not export: per-instance resolution
(`core/resolve.go` `resolveInputs` and `TopNode.resolve`), fork expansion
(`core/fork.go`) and the disable test (`core/stage.go` `Fork.disabled`). For
specialization, projection, narrowing and validation it calls Martian's exported
functions (`ResolvedBinding.BindingPath`, `LazyArgumentMap.Path`, each type's
`FilterJson`, and the validators). `TestMrpCallGraph` (e2e) replays every
mrp-diff fixture's real mrp pipestance through these subcommands, running every
job through `mre run`. It requires the same `_args` as mrp's, after both are
re-encoded and file leaves are reduced to name and content digest. Every job
mrp ran must run, and no other, with the same `_args`, `_outs` and `_jobinfo`
(its keys, and every value that does not vary per run). The published tree and
pipeline outs must match. Its drift guard requires the same `_args` and fork
set as mrp's own resolver, reattached read-only in-process.

---

## 7. Bundles

A **bundle** is mre's container for "a set of typed values, including the files
they reference": an args bundle, an outs bundle, a per-output field bundle, a
chunk's args, a fork element. On disk it is a directory:

```
<bundle>/
  data.json            ← the typed JSON sidecar (scalars, arrays, maps, structs)
  f/L0000/reads.fastq  ← each file-typed leaf in its own ordinal dir (the
  f/L0001/reads.fastq    canonical type-walk order), so leaves never collide,
                         under the basename mrp passes the stage (#284);
                         data.json references them via a marker
  f/L0000/.mre-origin  ← the leaf's origin (#431)
  instance.json        ← the instance stamp (args, outs and field bundles)
  fork.json            ← the fork record (fork element dirs)
```

The basename is the one the leaf's path had where `MarkFiles` found it: the file
a stage wrote (Martian's `<out>.<ext>` default, or any path the stage chose), a
split's chunk-def file, or the upstream leaf a binding forwarded unchanged. That
is the path `mrp` passes on, so a stage that reads a sample name or an extension
from `os.path.basename()` sees the same value under both runtimes. A task stages
its inputs as whole bundle dirs under `in/u*` names, so a stage-chosen basename
never appears in Nextflow's stage-in script.

In `data.json`, every file-typed leaf is replaced with a **marker** of the form
`@mre:file:f/L<nnnn>/<basename>` — a *bundle-relative* path. A leaf staged from a
**directory** (a `path` output, or a directory a stage wrote into a `file`-typed
out) uses `@mre:dir:` instead, so its ground-truth dir-ness — stat'd at staging
time — travels to publish rather than being inferred from the declared type.
Both prefixes resolve identically on read-back. The rules:

- **On write** (a stage produced an output that is a real file): the file is
  copied into the bundle's `f/` tree and the leaf is *relativized* to a marker.
  This is `shim.MarkFiles`, which uses the type manifest to find exactly which
  leaves are files (recursing into structs, arrays and maps).
- **Origins** (on write): a file is one to `mrp` wherever it is referenced, so a
  bundle copies it once and gives every position that reaches it one marker,
  however many input bundles it arrived through. Its identity is its origin, the
  absolute path it was first staged from, where its task wrote it, written to
  `.mre-origin` beside the leaf; a leaf of a staged bundle, such as the input a
  pass-through stage returns, keeps the origin recorded beside it, and a path
  inside a staged directory leaf takes that leaf's origin joined with the path
  below it, so a file inside a directory the run was given stays linked. The record
  also says how `mrp` publishes the file (`linked:` before the path): a stage
  output that was a symlink, or a file from outside every task (a host file,
  or one the run was given), is linked into `outs/` at each output that
  reaches it, where a stage's own file is moved to the first and the others
  link to it (`post_process.go` `moveOutFile`, `copyOutSymlink`). A leaf named
  `.mre-origin` fails the write. `mre outs` places each file of the pipeline
  outs in the pipestance it reconstructs by its origin (§6).
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
  the read. `mre outs` reads the pipeline outs this way, so the published outs
  carry the original strings.

**Symlink dereference.** Nextflow stages inputs as *symlinks*. When `mre` copies
a file leaf into a bundle, it dereferences the symlink to the real target first
— otherwise the bundle would carry a link that dangles once staged into another
task (`shim.CopyTree`). For a stage's own outputs the dereference is confined
(`shim.Confinement`). An output, and every symlink it passes through, is
classified as the stage's own file inside the task work dir (hard-linked into
the bundle), a path in the work dir outside the stage's phase dir, i.e. a staged
input (followed wherever it points, hard-linked), or a host path outside the
task:

- Under `-confine-outputs` (`mre run -confine`), such a path cannot be valid in
  an isolated container downstream, and copying it would upload host data, so
  the task fails, naming the output.
- Otherwise, where `mrp` publishes a symlink and pipelines legitimately return
  paths into host reference dirs, the target is staged as a byte copy, never a
  hard link, so no later write through the bundle can alias the host file.

---

## 8. Pipeline inputs: run parameters

The top-level `call EP(reads = …, scale = …)` provides the pipeline's default
inputs, which the project's copy of the entry `.mro` (`_assets/mro/`) carries.
You can override inputs **at launch** without re-transpiling:

- Every entry input is a nullable Nextflow parameter, `params.<name>`; null
  keeps the transpiled value. Set them with `-params-file inputs.json` or
  `--<name> <value>`. PLAN hands them to `mre plan` as one JSON object, which
  overlays them on the invocation data and recompiles the call, so a launch
  value can change the run's forks, gates and pruned calls as an invocation
  literal would under `mrp`.
- A command-line `--<name> <value>` reaches `mre plan` as a string, so a scalar
  `int`, `float` or `bool` input is parsed strictly into its declared type (a
  base-10 int64; a JSON number; exactly `true`/`false`). A value that does not
  parse, or a string for an array, map or struct input, fails the run; set those
  with `-params-file`.
- Every float in a supplied value, typed or parsed and at any depth, is
  re-encoded the way `mrp` encodes a float literal (`'g'`, shortest form). That
  covers a `float` leaf of any shape and, inside an untyped `map`, any number
  written with a `.` or an exponent, so `--f 2.0` and a `-params-file`
  `{"f": 2.0}` both reach the stage as `2`, as `f = 2.0` does under `mrp`.
- The string guarantee holds only under the strict (v2) syntax parser, the
  default from Nextflow 26.04. The v1 parser (the default on earlier releases,
  or `NXF_SYNTAX_PARSER=v1`) converts CLI values before the script sees them:
  `--b True` becomes `true`, and `--s 007` becomes the integer `7`, which no
  later step can recover. On such a launch, set
  `NXF_DISABLE_PARAMS_TYPE_DETECTION=true`. `-params-file` values are
  unaffected.
- An entry input named `outdir` or `job_resources` collides with a param the
  project defines itself and fails the transpile.

**File-typed inputs.** Every file value of the run's entry inputs that is an
absolute path or a URI (`s3://`, and others under the opt-in below) is staged
by Nextflow into the tasks that read it, and only those: PLAN lists the values
(`leaves`) and, per node and fork of its entry-fed roots, the ones Martian's
specialization of its bindings to that fork reads (`files`), so a map call over
entry files stages each fork's own element. The driver hands each to
`file(x, glob: false)`, so the head node never globs a name, and a task stages
its list as `entry/<i>/<name>`; `mre args -leaves` maps each value to the path
it reads it at. Nextflow links a file it can reach in place, so on a shared
filesystem, and in a docker container, which mounts the link's target, the
stage gets the value's own path, as `mrp` passes it; a file Nextflow fetched
(an `s3://` object, a file on an object-store work dir) is read where it was
staged. A value with no file (`[]`, `{}`, a null field) stages nothing. A
relative path names nothing from the stage's job dir under either runtime and
reaches the stage as given. A file default is staged from where it is at run
time; it is never copied into the project.

**Names Nextflow cannot stage.** Nextflow's stage-in script, on 24.04 and
26.04, writes each staged file as `ln -s <source> <name>`, leaving `*`, `?`,
`[`, `]`, `{`, `}`, `$` and control characters such as a tab for bash to
expand: `s[1].txt` beside `s1.txt` stages `s1.txt` silently. So PLAN refuses a
value holding any of them, naming the input and the character, before anything
is staged (a Nextflow staging limitation); rename such a file.

**Which file values a run may stage.** `file()` on the head node fetches any
URL Nextflow has a provider for and reads any path the head node sees, and a
run parameter comes from whoever launches the run. So PLAN checks every value
it lists: an `s3://` URI is staged on every profile; a head-node path (absolute,
or a `file:` URI) only where `params.mro2nf_host_inputs` holds (the local, grid
and docker profiles); any other scheme (`http(s)://`, `ftp://`) only with
`--mro2nf_allow_remote_inputs true`. The errors name the input, not the value,
which may carry a credential. The HealthOmics parameter template declares
neither param, so a HealthOmics run cannot set them.

---

## 9. Executors and resources

The project runs on a local or shared filesystem, under the `standard` profile
(the local executor) or a grid profile (`-profile slurm`, `sge`, `lsf`, `pbs`).
Isolated-container and cloud targets are tracked by #489.

- A stage's `using (threads = …, mem_gb = …)` sets each task's `cpus` (at least
  1, rounded up) and `memory` (1 GB when unset); a chunk and a join take the
  resources the split returned for them (§5). `_jobinfo` reports the request
  as `mrp` resolves it, unrounded.
- On the `standard` profile, `process.resourceLimits` clamps a request to the
  host's cpus and memory, so an oversized one runs rather than fails; a `-c`
  config setting `process.resourceLimits` overrides the clamp.
- A job's special, the stage's or the `__special` its split returned, selects
  `params.job_resources[special]` as the task's `clusterOptions`;
  `special = "gpu"` / `"gpu:N"` also requests N GPUs for that job.
- A `-c` config with `withName:` selectors on the node aliases retunes one
  stage at launch.

---

## 10. Faithful vs. divergent, and how it's validated

The transpiler aims for **byte-identical outputs to `mrp`**, and every supported
feature is checked against the real `mrp` output. The validation tiers:

- **Unit tests** (`make test`) — the call graph, resolver, topology, plan and
  posted records, the type walk and bundles, the generated project, and a
  stand-in run of every fixture's call graph through the `mre` subcommands.
- **Local e2e** (`make test-e2e`) — the golden fixture table transpiled, run
  under Nextflow, and diffed against committed `mrp` goldens; the live `mrp`
  differential (`TestMrpDiff`: published tree, `_outs`, and every stage's
  `_args`, `_outs` and `_jobinfo`); failure parity (`TestMrpDiffFailureParity`:
  the fixtures `mrp` fails fail the same way); and the call-graph replay of
  real `mrp` pipestances (`TestMrpCallGraph`).
- **Generative differential** (`TestGenDiff`, `make gen-diff`) — random
  well-typed programs (`test/gen`) whose stub stages derive every output from a
  hash of their inputs, run under live `mrp` and under mro2nf; any difference
  in the published outputs, a stage's `_args`/`_outs` or the run outcome is
  shrunk to a minimal fixture. Every comparator rule of the divergence
  allowlist, issue-tracked ones included, excuses a difference there, though
  none is reported as excusing nothing; the ABI and outcome matchers, which name
  their fixtures, do not apply. `make test-e2e` runs a fixed smoke set of seeds;
  `make gen-diff` runs a seed range for the long tier, which the weekly nightly
  tier starts from a random seed and ends after `GEN_BUDGET` minutes.
- **Static lint** (`make lint-nf`) — every fixture's project under
  `nextflow lint`, errors and warnings alike.
- **Overhead** (`make bench`, nightly `make test-scale`) — tasks, staged bytes
  and driver heap at two widths per shape; the plumbing task count must not
  change with the width, and the stage tasks must equal `mrp`'s job count.

The known **divergences** are the allowlist `test/e2e/divergences.json`, which
the differential applies and which generates the
[divergence table of `FEATURE_COVERAGE.md`](FEATURE_COVERAGE.md#divergences-from-mrp).
Some change only how the outputs are reached: `local`, `volatile` and `retain`
are no-ops (no VDR / mid-run work-dir reclamation), the published `outs/` tree
is copied into place rather than moved and symlinked, and `-resume` is
Nextflow's content-addressed cache. Others are
real output or outcome differences, each with its tracking issue or the reason
it is kept. A construct the call graph cannot carry fails the transpile, and
an invocation the witness does not cover fails PLAN (§5) before any stage
starts.
