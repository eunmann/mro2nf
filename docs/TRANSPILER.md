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
   │      ▼                    │  ──────►  │  N0_X   mre stage: args, job, post  │
   │  witness call graph      │           │  N1_Y   … one process per stage node│
   │      │  topology          │           │  OUTS   mre outs: mrp's PostProcess │
   │      ▼                    │           │  each mre stage runs the real stage │
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
   │                  main.nf, subworkflows/, stage.nf, nextflow.config,
   │                  _assets/mro/
   ▼
nextflow run main.nf …  →  PLAN, a task per stage instance, then OUTS
```

Source layout:

```
cmd/mro2nf/      → transpiler CLI: .mro -> Nextflow project
cmd/mre/         → runtime shim: plan | stage | outs
internal/
  cg/            → Martian's compiled call graph: witness, topology, plan,
                   the _args resolver, fork elements, gates, pipeline outs
  cgnf/          → call graph -> Nextflow project (main.nf wiring, stage.nf)
  job/           → runs one stage job through mrjob as mrp runs it
  types/         → type-directed file-leaf walk over Martian's compiled types
  shim/          → bundle I/O, markers, output confinement
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
- `-version`.

Everything else is a launch param of the project (§4).

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
main.nf               ← PLAN, the nodes or the calls of their runs, then OUTS
subworkflows/         ← a workflow per run of nodes (W<j>.nf), only in a project too large for one
stage.nf              ← the one hand-written stage module every node includes
nextflow.config       ← params, retry policy, GPU requests, trace, the target profiles
conf/                 ← the container profiles' and the HealthOmics profile's configs
package.sh, parameter-template.json ← the HealthOmics package (§9)
_assets/mro/          ← a copy of every .mro file the entry compiles from, each
                        at its absolute path below it; PLAN and OUTS compile it
```

**`stage.nf`** declares `STAGE` (a non-split stage) and `STAGE_SPLIT`,
`STAGE_MAIN` and `STAGE_JOIN` (a split stage's three phases), plus `OUTS`.
The workflow that wires a node includes them once for it under its own alias
(`include { STAGE as N3_DBL }`, a split stage as `N1_SUM_SQUARES_SP`, `_MN`
and `_JN`), so a `withName:` selector names the node. Every stage task runs
one `mre stage` call (§6), which compiles the call graph once and runs the
phase's job as `mrp` runs it, resolving the instance's args before it in
every task, and posting its outs after it in a join and a non-split stage's
main. A split's chunk mains and join stage the split's inputs, the same
instance tuple, and build its args again. Each process declares only the
outputs its consumers stage, as one output: a posting process emits its
outs, forks and gates as one tuple (`posted`), since Nextflow's DAG holds
an edge per declared output whether a node reads it or not (602 of
CellRanger's edges were outputs no node read). The steps of a call hand
each other the instance, its args and its outs in memory. `stage.nf` also
holds the wiring: sub-workflows that read PLAN's records (`NODES`, `ENTRY`,
`ENTRY_FORKS`, `ENTRY_ROOT`) and that take a node's
instances through each step of it (`FORK`, `ZIP`, `GATE`, `ENABLED`, `READ`, `GATHER`,
`INPUT`, `CHUNKS`, `JOIN`, …), each given the node's static data, so a
node's wiring is a few lines and one per root, read or gate of it. The driver runs only those and a few Groovy helpers (`ordered`,
`resources`), which read only records mre wrote; it parses no
stage-controlled JSON.

**Workflows.** The JVM compiles each workflow's statements into one method,
which may not pass 64 KiB of bytecode, and a call into one of at most 255
arguments. Only that bound shapes the workflows; the `.mro`'s pipeline
nesting does not. `main.nf`'s entry workflow wires every node, in the
dependency order of the call graph's nodes, beside PLAN and OUTS. When
they weigh more than it holds (`maxWeight`), the nodes are cut, in that
order, into runs, each filled until the next node would make it weigh more
than `maxWeight` or take more than `maxTakes` channels, and each is a
workflow `W<j>` of its own (`subworkflows/W<j>.nf`), which the entry
workflow calls in turn (and, were the calls of the runs themselves too
heavy for it, runs of those runs). A run takes the channels its nodes read
from outside it, emits those read outside it, and emits `outs_pub`: one
channel that mixes the outs bundles of its own nodes the pipeline outs
read (`PUBLISHED`). The entry workflow's `outs_pub`, which mixes its own
nodes' bundles with its runs' `outs_pub`, is OUTS's input, which `ordered`
sorts by node and instance key, so where a bundle is mixed does not change
it, and no workflow mixes more sources than it has calls. PLAN declares the
same outputs, and the entry workflow the same few statements of them,
however many entry-fed roots a pipeline has, as PLAN writes all their forks
as one record (§5). So no workflow grows
with the pipeline (`TestEmitBoundsEveryWorkflow`, `TestEmitBoundsPlan`,
`TestWidePipelineRuns`, `TestEntryRootsMatchMrp`). A process's trace name
is its alias, `N5_X`, in the entry workflow, and `W1:N5_X` in a run; a
`withName:` selector of its alias alone names it in either. Nextflow's task
hash includes that name, so `-resume` finds no cached task of a node that a
re-transpiled project wires in another workflow than the run did.

**`nextflow.config`** holds:

- `params.outdir` (`results`), and `params.job_resources`, Martian's
  `MRO_JOBRESOURCES` analog: a map from a stage's `special` key to the
  `clusterOptions` of a grid executor.
- `params.mro2nf_mre`, `mro2nf_mrjob`, `mro2nf_adapters`, `mro2nf_psid`,
  `mro2nf_monitor` and `mro2nf_confine`: the tools and knobs the stage module
  reads, PLAN refusing a psid `mrp` refuses (letters, digits, `-` and `_`
  only). Every task runs the `mre` on `PATH`; `mre` runs the `mrjob` beside
  its own binary, as `mrp` runs the `mrjob` beside its own, and the adapters
  it embeds (§6). A run names other tools with `--mro2nf_mre`,
  `--mro2nf_mrjob` and `--mro2nf_adapters`, such as a pipeline bundle's own
  Martian runtime, so the project names no host path of the tools.
  `--mro2nf_monitor true` is `mrp`'s `--monitor`: `mrjob` enforces each job's
  memory and thread limits. `--mro2nf_confine true` fails a stage task whose
  output resolves outside its work dir instead of copying it in (§7) on the
  local and grid profiles too; the container profiles always confine. A
  boolean param is on only when it is `true` or the string `'true'`, as a
  launch's `--<name> <value>` sets it under the v2 parser or
  `NXF_DISABLE_PARAMS_TYPE_DETECTION` (§8).
  `mro2nf_host_inputs` and `mro2nf_allow_remote_inputs` are the entry file
  values a run reads (§8), `mro2nf_host_in_place`, which the container
  profiles turn off (`conf/container.config`), whether every task reads a
  head-node path in place or Nextflow stages it (§8), `mro2nf_s3_mount` the
  root under which the
  `awsbatch` profile reads `s3://` values of mounted buckets in place (§8), and `mro2nf_image`
  the one image the container
  profiles run every task in, which they require (`mro2nf_require_image`,
  §9). `mro2nf_overrides` is the run's `mrp --overrides` file (§9).
  `mro2nf_local_jobs`, which the `standard` and `docker` profiles set, grants
  each job as `mrp`'s local job manager grants it, from the pool of the cpus
  and memory `process.resourceLimits` clamps its task to; elsewhere a job is
  granted as `mrp`'s cluster job managers grant it (§9).
- One `params.<name> = null` per entry input (§8).
- The retry policy, `mrp`'s: a task is retried, up to twice, only on exit 75,
  the failures `mrp` retries (§6), or on an executor kill or timeout (104,
  130–145, the exits nf-core pipelines retry), such as the executor's
  out-of-memory kill. An ASSERT (42), a stage exception
  or a failed `mre` step ends the run on its first attempt. A retry requests
  the memory and cpus of the first attempt, as `mrp` retries a job at its
  request: it escalates only under `--auto-adjust-memory`, which `mro2nf`
  does not support. Under `--mro2nf_monitor true`, `mrjob` kills the stage at
  its own request as `mrp` does, and `mrp` does not retry that kill.
- One `accelerator` rule, evaluated per task, that requests N GPUs for a job
  whose special, as `mrp` resolves it, is `"gpu"` or `"gpu:N"`: the stage's own,
  or for a chunk or the join the `__special` the split returned for it (see
  [`RUNNING.md`](RUNNING.md#gpu-stages)).
- Task tracing, and the target profiles (§9): `standard` (local, each job
  granted from and each task clamped to the host's cpus and 90% of its memory
  with `process.resourceLimits`), `slurm`, `sge`,
  `lsf`, `pbs`, `docker`, `awsbatch` and `healthomics`.

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
its presence. Its records reach the driver as values: the run's call, an
`eval` value of one line; and files the driver reads into values, as
Nextflow joins an `eval` output line by line in time quadratic in its lines:
per node a digest of its resolved spec (stage, bindings, disable bindings and
fork roots in Martian's JSON form) and of what its post step writes for its
readers (the splits of the roots it feeds, its partners' splits, and the
disable bindings whose value it posts with
their roots), whether the invocation disables it, the
fork keys a static per-fork binding disables, and its resources under the
run's overrides file (§9); the entry file values tasks stage (§8); and in
one record the forks of every entry-fed
root, keyed by the witness's outer roots, each with its fork record and its
index token. Each line of it leads with its root, and a node's wiring takes
its root's own lines from it (`ENTRY_ROOT`, a lookup by root).

**Cache identity.** PLAN caches on its inputs: the entry values, the `.mro`
copy, the overrides file and its meta, whether head-node values are read in
place, and the run's work dir, which no entry value read in place may hold
(§8). Under an S3 mount it does not cache,
as which s3:// inputs it reads in place depends on the buckets its instance
mounts (`TestResumeRechecksTheS3Mount`). A task's `-resume` key holds its own
node's digest and resources (in `meta`), the run's call, the mro2nf
build, and its fork records, value records and staged dirs. No task
stages a file of PLAN's: the digests reach it as values, which Nextflow
keys on by content, and so does the call, by one rule (`CALL`): a call
whose base64 is at most 4 KiB (`cg.MaxValueBytes`, the bound of an output
carried as a value) is a value each task's script writes into `call.b64`,
as on AWS Batch a staged file costs each task a download; a larger one is
a file the driver writes from PLAN's value once per run (`collectFile`),
in a dir named by the call and the run's session, which a `-resume` keeps,
and each task stages as `call.b64`. The call grows with the entry values,
so with N entry forks an inline call would grow every task's script, and
the key Nextflow hashes from its inputs, by O(N) bytes. A task still reads
the whole call, as `mrp` writes the whole invocation into every job's
`_jobinfo`, and keys on it, as `mrp` refuses to resume a changed
invocation. So a re-transpile whose `.mro` edit
leaves a node's spec and the call as they were re-runs PLAN but keeps the node
cached (`TestResumeKeysOnlyOnATasksOwnTypes`). An edit of how a consumer
splits or gates on a node's outs changes what the node's post writes, so it
re-runs the node too (`TestResumeKeysOnTheReadersAPostWritesFor`). A changed
entry value changes the call, and so re-runs every task. An edited overrides
file re-runs the tasks of each node whose resources it changes, a split
stage's split too, which writes the table its chunks and join are scheduled
by (`TestResumeKeysOnANodesOverrides`).

**Code.** A stage task compiles the run's call over the entry `.mro` where the
run finds its code: its includes and its stages' code resolve beside it, as
under `mrp`. `mre stage` fails the task when the node compiles to another spec
than the digest PLAN planned (`cg.ErrSpecDrift`): a `.mro` edited since the
transpile, or an image built from other sources, fails loudly rather than run.

**Instances.** An instance's key is the ordinal of its fork of each of its
roots. The driver enumerates a node's instances root by root: an entry-fed
root's forks come from PLAN, a stage-fed root's from the elements its
producer posted, `forks/<root>/<ordinal>/` dirs and their value records, and
each instance joins the forks agreeing on the outer roots. An element of a
type that holds no file leaf, whose JSON is at most `cg.MaxValueBytes`,
reaches the instance as a value record, under the same rule and with the
same bytes as an output does (**Reads**); any other it stages. An instance
zipping a stage output over a root, other than
the root's own source, joins that stage's partner element at its own fork's token,
whether that stage feeds the root or not, or the absent record, so each
fork gets one element of it whatever the fork width (the `zip` bench lanes
gate the bytes one task stages flat in N). A split of an output of a stage a
runtime gate disables, `split Q.o unless G.f`, is zipped the same way: the
instance reads the gate, and `Q`'s element only where the gate holds `Q`
enabled (the `zipgated` lanes). A disable binding splitting another stage's
output over an entry-fed root, `disabled = split FLAGS.skips`, is zipped the
same way: the driver gates on what FLAGS's post posted, and an enabled
instance resolves the binding again from its one element (the
`forkdisabled` lanes).

**Preflights.** As in `mrp`, each instance waits until every instance of each
preflight it is gated by is done: a pipeline's preflights gate every other call
in it, and through a sub-pipeline call every call within, its preflights too
(`preflight_order`). A disabled preflight instance counts as done, and a failing
one ends the run before the calls it gates start (`preflight_fail`). The wait
is on PLAN's node records, which every instance's gate reads (`GATE`): `AFTER`
holds them until a preflight is done, once per set of preflights some node
waits for, each set after the one it extends, and a node gates on the records
held for its own. Nextflow adds each edge of its DAG in time linear in the
edges before it, so a wait per node, which cost CellRanger three edges per node
and preflight, would make the launch quadratic in the nodes; this costs a few
edges per preflight (`TestEmitWaitsOncePerPreflight`). `TestLaunchEdges`
bounds the edges `-preview` builds for a wide pipeline and
`cellranger_shaped`, so an edge more per data dependency fails it.

**Gates.** A node's gates are decided on the driver: PLAN's record and each
gating stage's posted `gates` (an `eval` output), joined on the roots they
share. Each is parsed once into a set of the fork keys or gate tokens it
disables, PLAN's per node (`NODES`) and a gating instance's however many
nodes it gates (`GATES`), so an instance's gate is a lookup, not a scan of
its node's N forks. `GATE` keys each instance by its ordinals of the roots
of the stage its node's first gate joins on, and a gate is one combine
(`GATED`), which adds the gates that stage posted there. `REGATE` keys the
instance again, deciding it from the gates since (`disabled`), only before
a gate joining on other roots, and `ENABLED` decides it from the last, as
`REKEY` and `INPUT` do a node's reads (**Reads**): a map per gate to key
the instance cost CellRanger 831 edges of its DAG. A disabled instance runs no task and stands in with no
outs and no forks; Martian's call graph already reads its outputs as null in
every consumer.

**Reads.** An enabled instance gets, of each node it reads, only the outputs
it reads: from the instance that shares its roots, or, a gather, from every
such instance under it. A node's outs are keyed by its whole key, which an
instance sharing every root of it holds, so `READ` combines the node's outs
channel as it comes: a step keying or selecting them per read cost
CellRanger 1,263 edges of Nextflow's DAG, whose launch is quadratic in its
edges (**Preflights**). An enabled instance leads with its ordinals of the
roots its node's first read joins on (`ENABLED`), and a read is one
combine, which adds the node's outs after what the instance read before.
`REKEY` keys the instance again, with the outputs each read since reads
(`readItems`), only before a read joining on other roots, and the
process's `INPUT` picks those of the reads after it, so a run of reads on
the same roots costs no map per read, where a map per read cost
CellRanger 2,439 edges more. An output of a type that holds no file leaf, whose
JSON is at most 4 KiB (`cg.MaxValueBytes`), reaches it as a value: a record
its producer posted (`-field-values`), an `eval` output, that the driver
carries in its channels and the task writes into a file for `mre stage
-values`, so the record is part of the task's `-resume` key. Any other
output it stages as a bundle (`outs/<output>/`). A staged bundle costs each
reader on an object store a listing and a copy whatever its size (on the
CellRanger Batch run, 287 of 896 downloads were such outputs); a value costs
the driver its bytes per instance until its readers are done, which the
bound keeps from growing with an output's size. A gather is a seeded `groupTuple`
over all of its upstream, so an instance under which there is none gathers
none, and a forked instance waits for every fork of the node it reads, as `mrp`
waits for every fork of a node before a node reading it runs
(`martian/core/node.go` `getState`; `TestEmitForkedGatherWaitsForItsUpstream`).

**Files by reference.** Every bundle a stage task writes for another task
to stage, a posted outs or element bundle, a split's chunk def or a
chunk's outs, names a file its task staged, a whole leaf of a staged
bundle (a join's outs naming its chunks' files, a stage returning its
input, a split forwarding its input into each chunk's def), by reference
rather than as a copy of its own (§7): the task lists, per bundle, the
item the file lies in by the path the task staged it at (`in/u<i>`,
`refs/<i>`, `chunk/<name>`, `chunks/<name>`, `chunkouts/<name>`) and the
file's ordinal dir there (`-field-refs`, `-fork-refs`, `-out-refs`).
`POSTED` joins each posting task's outputs, by instance key, to the paths
the driver staged its items from (`SOURCES`, `JOIN_SOURCES`; `in/u?` names
the i-th of the list by its index), and resolves each record to the
ordinal dir in the producer's work dir, which the item carries beside its
dir. A task's records come on one line, space-separated, so an `eval`
output of them, whose records grow with a join's chunks or a fork root's
forks, is collected in time linear in them. A split's come in a file,
`chunk_refs`, beside `resources.tsv`, a chunk's as an `eval` output:
`CHUNKS` and `JOIN`, which read the split's outputs joined to the
instance's staged items (`SPLITS`), resolve them against the
paths the split, and each chunk, staged its items from (`chunked`,
`joinRefs`), so each chunk stages the files its def names and the join
those every chunk's def and outs name. A reader, OUTS included, stages
those ordinal dirs once each as `refs/<i>/f/<ordinal>/` (`refsOf`), so
Nextflow stages the file from where its producer wrote it, a link on a
shared filesystem and a download on an object store, and the writing
task uploads none of it again (on CellRanger on Batch the `MAKE_SHARD`
join spent 31.8 s re-uploading its chunks' shards, #562; a split
forwarding its input into M chunk defs uploaded M copies of it). An
ordinal dir that lies in an item the reader stages whole, such as the
input a split forwarded into its chunk defs, which each chunk and the
join stage as `in/u<i>`, is not staged again (`unstaged`): `mre`
resolves a reference among the leaves of every item its task staged,
each beside its origin record (`shim.Staged.Leaves`), so the file is
downloaded once per reader (`split_forward_file` with a 10 MB file,
copy-staged: 21.0 MB staged per chunk and for the join before, 10.5 MB
after). A
reader's `-resume` key holds the ordinal dirs' paths, which a cached
producer keeps. Nextflow's own `includeInputs` cannot do this: an output
it matches is named in the task's own work dir and is unstaged from it
like any other output (`TaskFileCollector`,
`AwsBatchFileCopyStrategy.getUnstageOutputFilesScript`), and a posted
bundle holds its files below a dir of its own. A file inside a staged
directory leaf and an entry file are still copies.

**Split stages.** A split's outputs are joined once to the instance its
task staged, by the key both hold at position 1 (`SPLITS`), and its chunks'
mains and its join read that join: joining them anew for each, through a
map of either side, cost CellRanger 6 DAG edges per split stage. A split's chunk outs are gathered by `groupKey` over the
chunk count. Each chunk's main and the join are scheduled with the cpus, memory
and special their jobs get, the split's `resources.tsv`, a file the driver
reads into rows without parsing a record, not an `eval` value, whose
collection is quadratic in its lines.

**OUTS.** The last task stages every outs bundle the pipeline outs read and
the files they name by reference, gets each bundle's location, and each
referenced file's bundle's, as a value (`toUriString()`, a path or an
`s3://` URI) and the outputs they read that reach it as values, and
publishes them with `mre outs` through `mrp`'s own post-processing: the
`outs/` tree and `pipeline_outs.json`, which `publishDir` copies into
`params.outdir`.

**Order.** Each staged list, and each list of value records, is in an order
the run fixes, a gather's and OUTS's by instance key, then output, and a
join's by chunk name, since list order is part of
a task's `-resume` key and arrival order is completion order.

**Overhead.** The tasks are PLAN, one per stage job `mrp` runs (each split,
chunk and join of a split stage, one per fork of a non-split stage), and OUTS.
Binding, forking, gating and gathering run inside those tasks or on the driver,
so the plumbing task count does not grow with fork width or chunk count
(`make bench`, `make test-scale`).

---

## 6. The runtime shim `mre`

Each subcommand that works on the call graph takes the entry `.mro`
(`-entry`, `-mropath`) and the run's call (`-call`, a file holding base64 of
the call PLAN rebuilt, which each task writes or stages, as a wide run's
call would pass the kernel's 128 KiB limit on one argument; the entry's own call without
it), compiles the call over the entry's
declarations as `mrp` compiles an invocation (`ParseSourceBytes`, then
`MakePipelineCallGraph`) as the entry file, and reads only the items its task
staged (`-in`, an item or a dir of items). Each finds a bundle's file leaves
by the types the invocation compiled to: Martian's type table, which holds
every filetype and struct, and the struct of each callable's outputs.

- **`mre plan`** starts a run (§5). It rebuilds the run's invocation from the
  transpiled entry and the run's entry values, compiles it, checks it against
  the witness, vets the entry file values the run reads (§8), and writes the
  call every task compiles, one record per witness node, the entry files of
  each node's instances and of the pipeline outs, and the forks of every
  entry-fed fork root, a record each. With `-overrides`, it
  reads the run's `mrp --overrides` file with Martian's `ReadOverrides`,
  which fails on a key it
  does not know, and each node's record carries the cpus and memory of its
  first job (its split, or its one main) and an overrides file of the node's
  own, `{"<fqid>": {...}}`: of each phase the node runs, each resource
  Martian's `ApplyResourceOverrides` sets for it, found at the key nearest
  the node.
- **`mre stage -phase split|main|join -node <fqid>`** is the one `mre` call
  of a stage task. It compiles the call graph once and runs, against it, the
  steps below that the phase has, each handing the next the instance, its
  args and its outs in memory: every phase first resolves the instance and
  its args from what its task staged, each file leaf linked below `-args`,
  as no other task stages it; then runs its job; then a join, or a
  non-split stage's main, posts the outs its job wrote. A split's chunk
  mains and join stage the split's inputs and resolve the same args the
  split did. An instance its args resolve disabled, where its gates held it
  enabled, fails the task with exit 1 before any job runs, and it writes
  nothing. A failed job exits as the job step below says, and no post
  follows it; a failed args or post step exits 1. Only the files the job
  and the task's consumers read are written: `_args` names each file leaf
  at its link below `-args`.
- **The args step** (`-node <fqid> -digest <d>`) resolves one stage
  instance and its args, once the node compiles to the spec
  digest PLAN planned. The instance's fork comes from the fork elements it
  staged or got as value records (`-values`), or the fork records it got as
  values (`-fork`, base64 of a `fork.json`), and its values from its
  parents' stamped outs bundles and value records. The instance records the
  fork and each part's fork count, from which `mrp`'s fork id follows. A map call
  that zips several splits forks over its master source: a split of known
  length if one has it, else its first split in source order. The other splits
  resolve at the same index, and, as in `mrp`, read null past the end of a
  shorter source or from a null one, and drop a longer one's extra elements.
  The missing element is logged, as at `mrp`'s default enforcement level;
  `mrp --strict` is not supported.
- **The job step** runs the task's phase of the instance the args step
  resolved as `mrp` runs its job (`internal/job`). py and comp stages run
  under `mrjob`, built from the pinned module (`make build`), with `mrp`'s argv
  (`core/node.go` `runJob`):
  `mrjob <adapters>/python/martian_shell.py <code> <phase> <meta>
  <meta>/files <journal>`. Exec stages run directly. `-mrjob` defaults to the
  `mrjob` beside mre's binary, and `-adapters` to Martian's adapters mre
  embeds (`vendor-martian/python`, the pinned module's `adapters/python`),
  which a py stage's job writes below its work dir, as `_mre_adapters`.
  Before the job, mre writes what `mrp` writes, through Martian's own types and
  metadata writer: `_args` (the chunk or join def's resolved resources over the
  bindings), `_jobinfo`,
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
  (`Runtime.Resources`, Martian's resolution of the stage's `using` under the
  def the split returned and then the `-overrides` file, the node's own that
  PLAN wrote). A split stage's main writes its chunk's outs bundle (`-o`),
  which its join stages; a non-split stage's main and a join hand their
  job's outs to the post step. The files a job names in any of them are
  confined to the work dir (§7), strict under `-confine`, but a split's
  chunk defs and a chunk's outs name each whole leaf of a bundle the task
  staged (`-in`, `-refs`, `-chunk`) by reference, which `-out-refs` lists
  (§5, **Files by reference**), and a chunk and a join read them among the
  leaves they staged, by reference (`-refs`) or in an item. Beside the job's metadata mre writes `_mre_stage`, the name of
  the stage the job ran. The adapter never reads it; the `mrp` ABI differential
  (`TestMrpDiff`) uses it to group stage invocations without depending on the
  Nextflow task graph, and the failure-parity leg (`TestMrpDiffFailureParity`)
  to name the stage a failed task ran.
- **The post step** runs after the stage's last job, on the outs the job
  wrote. It checks them with
  Martian's `LazyArgumentMap.ValidateOutputs`, as mrp's `Fork.verifyOutput`
  does. Then it
  writes one element per fork of every root this stage feeds:
  `forks/<root fqid>/<ordinal>/`. The ordinal is zero-padded, in mrp's fork
  order (array index, or map keys in byte order). Each holds that fork's
  element of the source collection and `fork.json`, which records the real
  fork (keys as JSON strings) and its position. An element of a type with no
  file leaf and at most `cg.MaxValueBytes` of JSON is a value record in the
  `-fork-values` file, named as its dir, `<root fqid>/<ordinal>:` and base64
  of its fork record and the element's JSON; any other is that dir, the
  element a bundle with its file leaves staged beside `fork.json`. A map
  key's fork also carries the key's token, the hex SHA-256 of the key, after
  an `@` (an array fork's token is its index). For each
  output of this stage that a map call splits other than as its root's source
  (a map call zipping several outputs, of this stage or of others, or a
  disable binding splitting one), it writes one partner element per element,
  `forks/<root>@<node>.<output>/<token>/`, a dir or a value record by the same
  rule, and an absent record, `none`, a value record, which
  reads null where the output is null and a missing element, logged, at any
  index it lacks, as `mrp` reads it; a zip of maps at run time need not share
  keys (`zip_runtime_map`). An instance therefore gets only its own elements,
  and no key ever needs escaping. `mre plan` writes the forks the call graph
  already fixes (literal and entry-sourced sources) the same way, as fork
  records without values, which a task gets as values (`-fork`). A root whose fork set varies with an outer map call (its
  source, of unknown length, is forked on that call) gets its forks under each
  fork of that call. The posting task enumerates an outer call its own fork does
  not fix from what it staged, and prefixes the name with the ordinal of each
  such call, joined by dots. A root of known length is keyed by its own forks
  alone, whatever outer calls its stages are also forked on. It writes the
  outs output by output, so a consumer gets only the outputs it reads: one
  of a type with no file leaf and at most `cg.MaxValueBytes` of JSON as a
  record in the `-field-values` file, `<output>:` and base64 of the
  instance's stamp and the output's JSON, and any other as one bundle
  stamped with the instance (`instance.json`), `<-fields>/<output>/`, the
  files it names staged as the job's own, confined (§7), but each whole
  leaf of a bundle the task staged (`-in`, `-refs`, `-chunk`, `-chunks`,
  `-chunkouts`) named by reference, which `-field-refs`, and for element dirs
  `-fork-refs`, list (§5, **Files by reference**). A consumer's `-values`
  file holds the records it got, of outputs and elements alike, each naming
  the item it stands in for in its JSON, and its `-refs` dir the files its
  items name by reference that no item it stages holds; `mre outs` takes
  both too. With `-gates`, it writes the
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
mrp-diff fixture's real mrp pipestance through these subcommands, running
every task through `mre stage` as a run's tasks do. It requires the same
`_args` as mrp's, after both are re-encoded and file leaves are reduced to
name and content digest, and an instance `mre stage` refuses as disabled
exactly where mrp disabled the fork. Every job
mrp ran must run, and no other, with the same `_args`, `_outs` and `_jobinfo`
(its keys, and every value that does not vary per run). The published tree and
pipeline outs must match. Its drift guard requires the same `_args` and fork
set as mrp's own resolver, reattached read-only in-process.

---

## 7. Bundles

A **bundle** is mre's container for "a set of typed values, including the files
they reference" that one task hands another: a per-output field bundle, a
chunk's args, a chunk's outs, a fork element. On disk it is a directory:

```
<bundle>/
  data.json            ← the typed JSON sidecar (scalars, arrays, maps, structs)
  f/L0000/reads.fastq  ← each file-typed leaf in its own ordinal dir (the
  f/L0001/reads.fastq    canonical type-walk order), so leaves never collide,
                         under the basename mrp passes the stage (#284);
                         data.json references them via a marker
  f/L0000/.mre-origin  ← the leaf's origin (#431)
  instance.json        ← the instance stamp (field bundles)
  fork.json            ← the fork record (fork element dirs)
```

The basename is the one the leaf's path had where `WriteItem` found it: the file
a stage wrote (Martian's `<out>.<ext>` default, or any path the stage chose), a
split's chunk-def file, or the upstream leaf a binding forwarded unchanged. That
is the path `mrp` passes on, so a stage that reads a sample name or an extension
from `os.path.basename()` sees the same value under both runtimes. A task stages
its inputs as whole bundle dirs under `in/u<i>` names, and the files they name
by reference that lie in none of those as ordinal dirs, `refs/<i>/f/L<nnnn>`, so a stage-chosen basename
never appears in Nextflow's stage-in script.

In `data.json`, every file-typed leaf is replaced with a **marker** of the form
`@mre:file:f/L<nnnn>/<basename>` — a *bundle-relative* path. A leaf staged from a
**directory** (a `path` output, or a directory a stage wrote into a `file`-typed
out) uses `@mre:dir:` instead, so its ground-truth dir-ness — stat'd at staging
time — travels to publish rather than being inferred from the declared type.
Both prefixes resolve identically on read-back. The rules:

- **On write** (a stage produced an output that is a real file): the file is
  copied into the bundle's `f/` tree and the leaf is *relativized* to a marker.
  This is `shim.WriteItem`, which walks the compiled types to find exactly
  which leaves are files (recursing into structs, arrays and typed maps).
- **Linked args** (on write): a task's args are read only by its own job,
  so `mre stage` links each of their file leaves below `-args`
  (`shim.LinkLeaves`) instead of copying it, and writes no payload: a leaf
  of a staged bundle by its ordinal dir, whose origin record comes with it,
  any other file by itself beside its origin record, and `_args` names each
  at its link. A file every fork of a map call reads, such as a broadcast
  upstream output or entry file, exists once, its producer's, however wide the
  fork (#498). A split's chunks and join build their own args from the
  split's inputs, which Nextflow stages into them, so no task outputs args
  and none carries a copy of the files they name (#507).
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
- **References** (on write): a post, a split's chunk defs and a chunk's
  outs name a file that is a whole leaf of a bundle their task staged,
  reached as Nextflow staged it or through the link of the task's args, by
  reference (`shim.WriteItem`): the marker `@mre:ref:<origin record>` in
  place of a copy, and the item it lies in and its ordinal dir for the
  driver (§5). The task detects it by the path it staged the item at, not
  by a link, so it holds on an object store, where Nextflow stages a copy.
  Identity is the origin, as for a copy: every position of one origin gets
  one reference, and a reader resolves it among the ordinal dirs it staged,
  in its items or by reference, each beside its origin record
  (`shim.Staged.Leaves`, the first of an origin in the order the task names
  its staged dirs), failing on one it did not stage. A reader of outs and
  elements then links it into its args, and `mre outs` places it, as it
  would a copy of the same origin, so `_args`, the published tree and the
  outs JSON are the same bytes. A chunk and a join read a chunk def's or a
  chunk's outs' reference where they staged it, in an item
  (`in/u<i>/f/<ordinal>/<basename>`) or by reference
  (`refs/<i>/f/<ordinal>/<basename>`), where a copy lay below
  `chunk/<name>/` or `chunks/<name>/`: the basename the stage sees, and the
  file, are the same. A path inside a staged directory
  leaf is still copied, as staging the whole directory to every reader
  would cost more than the one file.
- **Escaping** (on write): every other string that begins with the reserved
  `@mre:` prefix (a string input, a `.mro` literal, a stage's string output, a
  file-typed out naming no file) is stored as `@mre:str:<original>`. A marker in
  `data.json` is therefore always one `WriteItem` wrote at a file leaf the type
  walk found, never a user's string.
- **On read** (a stage is about to consume a bundle): markers are *absolutized*
  back to real paths inside the staged bundle, so the stage opens a real local
  file, and escaped strings get their original value back, so the stage sees
  exactly the string `mrp` would pass it. A marker is accepted only in the shape
  `WriteItem` writes (`f/L<nnnn>/<basename>`: exactly one name under the
  ordinal dir, not `.` or `..`, bundle-local), and a reference only naming an
  absolute origin; any other `@mre:` string fails
  the read. `mre outs` reads the pipeline outs this way, so the published outs
  carry the original strings.

**Symlink dereference.** Nextflow stages inputs as *symlinks*. When `mre` copies
a file leaf into a bundle, it dereferences the symlink to the real target first
— otherwise the bundle would carry a link that dangles once staged into another
task (`shim.CopyTree`). For a stage's own outputs the dereference is confined
(`shim.Confinement`). An output, and every symlink it passes through, is
classified as the stage's own file inside the task work dir (hard-linked into
the bundle), a path in the work dir outside the stage's phase dir, i.e. a staged
input (followed wherever it points, hard-linked), a path outside the task
inside what a staged input links to, i.e. an input's real path, which a stage
that resolves its args' paths returns (hard-linked, as the input it resolves
from), a path under one of the run's read-only roots (§8), the S3 mount or
an entry input every task reads in place, which the stage was given, by the
path given or its real path (copied
as a staged input; the path under the root itself, as an output, stays a
path, so a stage returning its input's path copies nothing), or any other
host path outside the task:

- Under `--mro2nf_confine true` (`mre stage -confine`), such a path cannot be
  valid in an isolated container downstream, and copying it would upload host
  data, so the task fails, naming the output.
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
- An entry input named `outdir`, `job_resources` or `mro2nf_*` collides with
  a param the project defines itself and fails the transpile.

**File-typed inputs.** Every file value of the run's entry inputs that is an
absolute path is read in place by every task on the local and grid profiles
(`EntryLeaf.InPlace`, below), and every other one, a URI (`s3://`, and others
under the opt-in below) or, on a container profile, an absolute path, is
staged by Nextflow into the tasks that read it, and only those: PLAN lists the staged
values (`leaves`) and, per node and fork of its entry-fed roots, the ones
Martian's specialization of its bindings to that fork reads (`files`), so a
map call over entry files stages each fork's own element. The pipeline outs'
own reads, keyed by the pipeline's fqid, are staged into OUTS, whose
post-processing publishes such a file only where it finds it, as `mrp`'s
does. The driver hands each to `file(x, glob: false)`, so the head node never
globs a name, and a task stages its list as `entry/<i>/<name>`; `mre stage
-leaves` and `mre outs -leaves` map each value to the path it reads it at.
The stage gets a staged value as a leaf linked below its args dir (§7),
`<task>/args/f/L<nnnn>/<name>`: the same file name and content, at another
path than the value, which `mrp` passes. A value with no file (`[]`, `{}`, a
null field) stages nothing. A relative path names nothing from the stage's
job dir under either runtime and reaches the stage as given. A file default
is read or staged from where it is at run time; it is never copied into the
project.

**Head-node paths read in place.** The `standard` and grid profiles run their
tasks on the host's filesystem, where every task sees the head node's paths,
so `params.mro2nf_host_in_place` is true there and PLAN (`mre plan
-host-in-place`, `cg.ReadInPlace`) marks every absolute value read in place:
a task reads the value where it is, and the stage gets the value itself, the
path `mrp` passes. PLAN leaves it out of `leaves` and `files`, while it keeps
its index among the values, which a task recomputes from the call. Every
task, told the same by `mre stage -host-in-place`, computes the run's
read-only roots (`shim.Roots`, `Graph.Roots`) from the call it compiles: each
value read in place, and the S3 mount below. Every bundle writer keeps a leaf
under a root as the path it is, with no link, copy or origin, so a stage that
returns its input's path, as Cell Ranger's `SETUP_REFERENCE_INFO` returns
`reference_path`, posts the path, and no byte of the input is copied into
`work/`. A path in the task's own work dir is under no root, so a stage's own
outputs stay its own even below an input directory; and PLAN, handed the
run's work dir (`-run-workdir`), refuses a value that is or holds it, which
every task would otherwise read as an input. A task's `-resume` key holds the
value's path, as `mrp`'s restart keys a pipestance on its invocation, not the
file's content: Nextflow keys a staged file on its path, size and
modification time, and a staged directory on the directory's own size and
time, which a change to a file inside it leaves as they were, so for the
directory inputs Martian pipelines take, the path is what a staged input was
keyed on too.

The container profiles turn the param off (`conf/container.config`), so a
head-node value there is staged as a URI is: a container sees only what its
task stages and mounts. Reading it in place on `docker` would bind it into
every container, as a typed output can carry an input's path to any later
task (`SETUP_REFERENCE_INFO`'s `reference_path`), so a task's own inputs do
not bound the paths it opens: a run-wide bind list costs every task one
mount option per value, quadratic in a map call's width over the run.
`awsbatch` and `healthomics` refuse a head-node value outright.

**Names Nextflow cannot stage.** Nextflow's stage-in script, on 25.10 and
26.04, writes each staged file as `ln -s <source> <name>`, leaving `*`, `?`,
`[`, `]`, `{`, `}`, `$` and control characters such as a tab for bash to
expand: `s[1].txt` beside `s1.txt` stages `s1.txt` silently. So PLAN refuses a
staged value holding any of them, naming the input and the character, before
anything is staged (a Nextflow staging limitation); rename such a file. A
value read in place is never staged, so it may hold any of them, as under
`mrp`.

**Which file values a run may read.** `file()` on the head node fetches any
URL Nextflow has a provider for and reads any path the head node sees, and a
run parameter comes from whoever launches the run. So PLAN checks every value
it lists: an `s3://` URI is staged on every profile; a head-node path, read
in place or staged, and a `file:` URI, staged, only where
`params.mro2nf_host_inputs` holds (the local, grid and docker profiles); any
other scheme (`http(s)://`, `ftp://`) only with
`--mro2nf_allow_remote_inputs true`. The errors name the
input, not the value, which may carry a credential. The HealthOmics parameter
template declares neither param, so a HealthOmics run cannot set them.

**Mounted `s3://` values.** A stage that records an input's path in an output
another task opens, as Cell Ranger does with its FASTQ directory, needs the
path `mrp` gives it, one path in every task; a staged copy is at a path no
other isolated task has. Under `params.mro2nf_s3_mount`, a root at which every
task's host may mount input buckets read-only (the `awsbatch` profile's
`/mnt/s3`, which Mountpoint for S3 serves on the example stack's instances),
PLAN (`mre plan -s3-mount`), which runs on the same compute environment as
every task, rewrites each `s3://<bucket>/<key>` file value of the invocation
whose bucket is a directory `<root>/<bucket>` to `<root>/<bucket>/<key>`, a
trailing `/` kept, before it compiles the call, so every task compiles a call
naming the mounted path. Any directory there counts, not only a mount point:
the e2e suite serves the root as one bind-mounted tree, and the example
stack's root holds nothing but its mounts. A value of any other bucket stays
its URI and is staged as on every other profile, which needs the tasks'
role to read its bucket (the example stack's reads only the buckets it
mounts), so the mount spares
downloads and makes a recorded path hold in every task, and no run requires
it. A value under the root, so written or given directly once cleaned, is
mounted: PLAN checks that the mount shows its bucket and key and leaves it out
of `leaves` and `files`, so no task stages it, while it keeps its index among
the values, which a task recomputes from the call, a mounted and a staged
value of one run alike. The root, which `mre stage -s3-mount` names, is one
of the run's read-only roots (above): every bundle writer keeps a leaf under
it as its path, with no link, copy or origin, and a strict confinement admits
a stage's own link into it (§7). OUTS publishes such a
value as a Linked file, as it does a host path. A key of a mounted bucket
with an empty, `.` or `..` component, which Mountpoint cannot show, is
refused. A task's `-resume` key holds the value's path, not its content.

---

## 9. Targets, executors and resources

**Targets are profiles of one project** (`nextflow run -profile <name>`): the
`.nf` files are the same everywhere, and a profile sets only which executor
runs the tasks and in which image; every task finds `mre` on `PATH`.

- Every profile finds the code where the project was transpiled from: a
  stage task compiles the entry `.mro`, its includes and its stages' code at
  their absolute transpile-time paths; PLAN and OUTS compile the project's
  `.mro` copy (§4) and need no code.
- `standard` (the local executor) and the grid profiles (`slurm`, `sge`,
  `lsf`, `pbs`) run on the host's `mre` and code, on a shared filesystem,
  where every task reads a head-node entry file in place (§8).
- The container profiles, `docker`, `awsbatch` and `healthomics`, include
  `conf/container.config`: outputs confined to the task (§7), a head-node
  entry file staged (§8), and every task, PLAN and OUTS included, in the one
  image
  `params.mro2nf_image`. They set `params.mro2nf_require_image`, so a launch
  without an image fails before PLAN.
- **The image.** `deploy/runtime/Dockerfile` puts `mre` and the `mrjob`
  beside it on the image's `PATH` at `/opt/mro2nf`, published as
  `ghcr.io/eunmann/mro2nf-runtime:<version>` by each release (its `v*` tag).
  The user's image builds `FROM` it and adds the pipeline's code at the paths
  the project was transpiled from (README, "Container image"); a stage `src`
  that names a command must be on the image's `PATH`. The runtime image
  itself serves where a `-c` config mounts the code at those paths. `mre
  args` refuses a node the image compiles to another spec than PLAN planned
  (§5), so a stale image fails loudly.
- `docker` runs each task in a container that mounts only what it stages.
- `awsbatch` runs on AWS Batch over an S3 work dir (`-work-dir s3://…`); every
  task stages its upstream inputs from S3, so nothing reads a head-node path,
  and a head-node path as an entry file is refused (§8). It reads an `s3://`
  entry file in place under `/mnt/s3` (`params.mro2nf_s3_mount`), the root it
  bind-mounts read-only into every container (`aws.batch.volumes`), where the
  compute environment's instances mount the file's bucket (§8,
  `deploy/awsbatch-cdk`), and stages it otherwise. Set `process.queue` and
  `aws.region` in a `-c` config; `--outdir s3://…` publishes to S3.
- `healthomics` (HealthOmics takes no `-profile`): `./package.sh
  <image>@sha256:…` zips the files `main.nf` reads, appends the image and
  `includeConfig 'conf/healthomics.config'` to the zip's `nextflow.config`, so
  the profile is the workflow's own, and publishes to `/mnt/workflow/pubdir`.
  The image must be pinned by digest: a workflow version then runs the code
  it was validated with, and as `parameter-template.json` declares only the
  entry inputs and `mro2nf_overrides`, no run can name another image.

- A stage's `using (threads = …, mem_gb = …)` is its jobs' request; a chunk
  and a join take the resources the split returned for them (§5). Each job is
  granted its request as `mrp` grants it in the jobmode the profile runs as
  (`job.Runtime.Resources`): on `standard` and `docker`, `mrp`'s local job
  manager's grant from a pool of `process.resourceLimits`' cpus and whole GB
  of memory, which PLAN and every stage task pass `mre` from their own task
  (`-local-cores`, `-local-mem-gb`), so an adaptive request gets the pool and
  every request but vmem at most the pool; elsewhere `mrp`'s cluster job
  managers' grant, a request at its magnitude in whole threads. `_jobinfo`,
  `__threads`, `__mem_gb`, `__vmem_gb` and the thread variables report the
  grant, and the task requests its threads rounded up as `cpus` (at least 1)
  and its memory as `memory`.
- `--mro2nf_overrides <file>` is `mrp --overrides`: PLAN reads it (§6), and
  every job's threads, memory and vmem resolve under it as `mrp`'s
  (`core.ApplyResourceOverrides`), over `using` and the split's request,
  into its task's `cpus` and `memory`, `_jobinfo` and `_args`. The driver
  parses none of it: each task gets its node's resources and own overrides
  file as values of PLAN's record, and a chunk and the join take theirs from
  the split's `resources.tsv`. `force_volatile` and `*.profile` change no
  output and have no effect ([`RUNNING.md`](RUNNING.md#resources)).
- On the `standard` and `docker` profiles, `process.resourceLimits` is the
  host's cpus and 90% of its memory, and clamps each task, so a `withName:`
  request larger than the host runs rather than fails; a `-c` config setting
  `process.resourceLimits` sets another pool. Without a cgroup cpu quota or
  memory limit that is `mrp`'s default `--localcores` and `--localmem`;
  under one, the JVM counts the quota's cpus where `mrp` counts every cpu in
  its affinity mask (`runtime.NumCPU`), and `mrp` may take 96% of the limit.
- A job's special, the stage's or the `__special` its split returned, selects
  `params.job_resources[special]` as the task's `clusterOptions`;
  `special = "gpu"` / `"gpu:N"` also requests N GPUs for that job.
- A `-c` config with `withName:` selectors on the node aliases retunes one
  stage at launch.

---

## 10. Faithful vs. divergent, and how it's validated

A transpiled pipeline publishes the same outputs as `mrp`, except for the
allowlisted divergences below. Every fixture `mrp` can run is checked against
the real `mrp` output; `exec_min` and `mixed_adapters`, which `mrp` never
completes, have goldens only. The validation tiers:

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
  `nextflow lint`, errors and warnings alike, and a generated pipeline's
  project large enough for `subworkflows/` modules, which no fixture's has.
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
