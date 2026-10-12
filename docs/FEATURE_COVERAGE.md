# Martian feature coverage

This matrix tracks every Martian feature (from the martian-lang.org docs and the
authoritative `martian/syntax` grammar) against the transpiler's support and the
test that exercises it. Test types: **e2e** = transpile + run under Nextflow,
output diffed against real `mrp` (the Go `test/e2e` suite: the committed golden,
and the live `mrp` differential `TestMrpDiff`, which also compares every stage's
`_args`, `_outs` and `_jobinfo`); **unit** = Go test; **corpus** = parse+lower
robustness sweep.

Provenance: this matrix was built from a systematic page-by-page audit of all
seven martian-lang.org docs (writing-pipelines, language-details, writing-stages,
advanced-features, storage-management, running-pipelines, inspecting-pipelines)
cross-checked against the `martian/syntax` grammar; each documented feature was
classified covered/partial/guarded/out-of-scope and gaps were closed with the
fixtures cited below. Generated output targets the latest Nextflow (verified on
26.04.x), DSL2, and lints warning-free (`make lint-nf`).

Status: ✅ supported & tested · ⚠️ supported with a divergence from `mrp`, every
one a row of [Divergences from mrp](#divergences-from-mrp), generated from the
allowlist the differential applies · ❌ unsupported, guarded with a clear error &
tracked · 🚫 runtime/`mrp`-only, out of scope for a transpiler.

**How unsupported features are signalled (never silent):** the call graph is
Martian's own, so a pipeline Martian's compiler or call-graph builder refuses
fails the transpile with Martian's message and writes no project, and a run
invocation it refuses fails the run's PLAN task before any stage starts. A
shape whose data the tasks cannot exchange (a fork root several stages feed, a
disable binding forked on a map call another stage feeds) fails the transpile
too.

## Language — writing-pipelines / language-details

| Feature | Status | Test |
|---|---|---|
| stage / pipeline / top-level call | ✅ | e2e `split_test`, all |
| top-level call targeting a bare **stage** | ✅ | e2e `stage_entry` |
| entry inputs as run parameters (override at launch) | ✅ each entry input → `params.<name>` (null keeps the transpiled value); PLAN's `mre plan` overlays a `-params-file` or `--<name>` value on the invocation and recompiles it, so a launch value changes forks, gates and pruned calls as a literal would; CLI strings are parsed into the declared scalar int/float/bool, and every override float takes `mrp`'s literal encoding (`2.0` → `2`) | unit `TestTypeOverrides`, `TestPlanNestedEntryForks`; e2e `TestCLITypedOverrides`, the golden `*_override` cases |
| file-typed entry inputs | ✅ a file value is a path passed through verbatim, as `mrp` passes it: an absolute path reaches the stage, a relative one names nothing from the stage's job dir under either runtime. Staging into isolated containers and remote (`s3://`) values: #489 | e2e `entry_file`/`entry_filearr`/`entry_struct_file`/`entry_mapfile`/`entry_dir`/`entry_file_empty` (`_override`: absolute paths; the defaults: `TestMrpDiffFailureParity`), `TestEntryFileAdversarialName` |
| pipeline with **no calls** (return-only) | ✅ | e2e `returnonly` |
| `@include` (resolved via `-mropath`, default `$MROPATH`, else the `.mro`'s directory, as `mrp`; never the working directory) | ✅ | e2e `include_test`; unit `cmd/mro2nf` `TestRunDefaultSearchPath`, `TestSearchPathReachesOptions`, `internal/cg` `TestSourceIncludesIgnoreWorkingDir` |
| `@include` diamond / cycle / missing | ✅ (parser-enforced) | via `martian/syntax`; corpus parse |
| `filetype` | ✅ | e2e `file_min`, `kitchen_sink` |
| `struct` types | ✅ | e2e `struct_min`, `struct_proj`, corpus |
| named / unnamed (`default`) out params | ✅ | e2e `default_out` |
| out filename (`out txt r "" "f.txt"`) | ✅ | e2e `file_min` |
| scalar types int/float/string/bool/path/file/map | ✅ | e2e (various) |
| typed arrays `T[]`, multi-dim `T[][]` | ✅ | e2e `multidim`, `split_test` |
| typed maps `map<T>` (output) | ✅ | e2e `typedmap_out` |
| int→float coercion; float→int rejected | ✅ | `martian/syntax` (corpus parse) |
| whole-number float literal → int param (`5.0` → `5`) | ✅ each bound value goes through its type's `FilterJson`, as mrp resolves it; a split-returned chunk arg reaches main and join as the split wrote it | e2e `float_to_int`, `chunk_float_int` |
| literals: int/float/sci, bool, null, arrays, maps, struct literals, escapes | ✅ | `martian/syntax`; e2e entry args |
| bindings: `self.x`, `self.x.y`, `STAGE`, `STAGE.out`, `STAGE.out.field`, `STAGE.default` | ✅ Martian's `ResolvedBinding.BindingPath` and `LazyArgumentMap.Path` | e2e `struct_proj`, `default_out` |
| field projection through arrays, maps and arrays of maps (`CALL.s.field`, `map<S>.field`, `array<map<S>>.field`) | ✅ Martian's own projection | e2e `struct_proj`, `map_struct_proj`, `arr_map_proj`; unit `TestMergeProjection` |
| literal edge cases: negative numbers, `>2^53` int64 precision, unicode/escape strings | ✅ | e2e `literals_edge`, `literals_big` |
| wildcard binding `* = self` / `* = REF` | ✅ | e2e `wildcard` (compiler expands) |
| **refs nested in array/map literals** (`[A.x, B.x]` fan-in) | ✅ | e2e `fanin` |
| struct duck typing (extra fields) | ✅ every bound value is narrowed to the receiving param's declared struct by its type's `FilterJson` (extra fields and their file leaves dropped, a missing field null), in stage `_args` and pipeline outputs | e2e `struct_narrow` |
| struct and map key order | ✅ #366 a value reaches a stage's `_args` and `pipeline_outs.json` in mrp's key order | e2e `struct_key_order`, `nested_struct`, `kitchen_sink`; unit `TestKeyOrder`, `TestRunPublishLayoutKeepsKeyOrder` |
| null inputs / null propagation | ✅ | e2e `null_in`, `modifiers_min` |

## Calls & modifiers

| Feature | Status | Test |
|---|---|---|
| aliased calls `call X as Y` (repeated) | ✅ | e2e `alias_min`, `kitchen_sink` |
| nested pipeline calls (pipeline→pipeline) | ✅ | e2e `modifiers_min`, `kitchen_sink` |
| non-linear / diamond DAGs | ✅ | e2e `diamond_min` |
| `using (disabled = ref)` (self or call ref) | ✅ decided per instance as mrp's `Fork.disabled` decides it: a gate that is null at run time leaves the call enabled; a gate the invocation fixes is PLAN's, one a stage's outs set is that stage's posted gates, both evaluated on the driver, so a disabled instance runs no task. A gate null before any stage runs fails with mrp's `disabled cannot be bound to a null value` binding error | e2e `modifiers_min`, `null_gate`, `chain_gate`, `split_gate`, `split_gate_entry`; unit `TestRuntimeDisable`, `TestStaticDisable`, `TestNullGateEnables`, `TestPostGates` |
| whole-call ref (`s = CALL`) to a **disabled** stage or pipeline call | ✅ null, as mrp | e2e `disabled_wholeref` |

## Map calls / forks — writing-stages / advanced-features

| Feature | Status | Test |
|---|---|---|
| `map call ... split` over an **array** → array result | ✅ | e2e `fork_min` |
| `map call ... split` over a **map** → keyed result | ✅ fork elements in mrp's order, map keys in byte order; a key of any characters crosses only inside mre's records | e2e `map_fork`, `map_key_sort`; unit `TestForkElementKeys`, `TestForkRecordRoundTrip` |
| zipped multi-`split` | ✅ forks over its master source; the other sources read null past a shorter one's end | e2e `multisplit`, `zip_runtime`, `zip_literal`; unit `TestZip*` |
| whole-call ref (`x = CALL`) to a **map call** → one struct per fork | ✅ | e2e `map_wholeref`; unit `TestWholeCallRefs` |
| empty split collection → null result | ✅ pruned as mrp prunes it, statically or at run time | e2e `empty_fork_*`, `runtime_empty_forks`; unit `TestEmptyForksPrune` |
| map call over a **split stage** | ✅ | e2e `map_split` |
| map call over a **sub-pipeline** | ✅ | e2e `map_pipe`, `map_pipe_split`, `fork_indep_call` |
| map call emitting a **file** (array/keyed-map of files) | ✅ | e2e `map_file`, `map_file_keyed`, `map_file_split` |
| **complex combo**: map over a sub-pipeline whose body is a split stage emitting a file (Cell Ranger per-sample pattern) | ✅ | e2e `map_split_file` |
| **complex combo**: py + exec + comp adapters chained in one pipeline | ✅ | e2e `mixed_adapters` |
| **complex combo**: struct output whose field is a file array | ✅ | e2e `struct_file_array` |
| empty-split fork inside a map call (0 chunks → 0/null, no fork dropped) | ✅ | e2e `map_split` (includes an empty fork) |
| `disabled` on a map call | ✅ decided per fork | e2e `disabled_map`, `disabled_map_ref`, `disabled_map_nullforks` |
| map call over a pipeline with an internal **disabled** call | ✅ | e2e `map_pipe_disabled` |
| **nested** map call (a map over a pipeline that itself maps) | ✅ a root whose forks vary with an outer map call gets its forks under each fork of that call | e2e `fork_indep_call`, `bench/mapnested`; unit `TestNestedEntryForkNames`, `TestNestedForkUnderRuntimeOuter`, `TestPlanNestedEntryForks`. `map_pipe_nested`, whose merge `mrp` refuses, fails as under `mrp` (`TestMrpDiffFailureParity`) |
| **disabled** nested map call | ✅ | e2e `map_pipe_disabled_nested` |

## Stages / adapters / resources

| Feature | Status | Test |
|---|---|---|
| split / main / join | ✅ one task per chunk, as mrp runs one job per chunk | e2e `split_test`, `kitchen_sink`; unit `TestRunPhasesOfASplitStage` |
| ordered chunk-outs aggregation in join | ✅ by chunk name, never completion order | e2e `jitter_wide`; unit `TestJoinWritesChunkFiles` |
| `py` adapter | ✅ through `mrjob`, as mrp runs it | e2e (all py); unit `TestArgvIsMrps` |
| `exec` adapter | ✅ | e2e `exec_min`, `src_args` |
| `comp` adapter (through `mrjob`) | ✅ | e2e `comp_split`, `mixed_adapters` |
| per-chunk resources `__mem_gb/__threads/__vmem_gb/__special` | ✅ in `_args`/`_jobinfo`, through Martian's own types; a value of the wrong JSON type fails the split, as under mrp (#275) | e2e `TestMrpDiffFailureParity` (`split_bad_special`, `split_bad_mem`, `split_bad_threads`), `join_resources`; unit `TestReqs` |
| per-chunk and split-returned join resources → Nextflow scheduler directives | ✅ the driver sizes each chunk and the join from the records the split wrote (`chunk.json`, `join.json`) | e2e `split_test`, `join_resources` |
| resources a stage sees (`__*`, `_jobinfo`) | ✅ #243 #370 each job's `_jobinfo` carries the request as mrp's local job manager resolves it, fractional memory and threads included, not the task's whole-unit `cpus`/`memory` | e2e `frac_mem`, `frac_threads` (mrp ABI diff); unit `TestMainWritesMrpMetadata` |
| `using(special)` → scheduler | ✅ `clusterOptions` from `params.job_resources[special]` (the `MRO_JOBRESOURCES` analog); `special = "gpu"`/`"gpu:N"` requests GPUs instead (`GPU.md`) | e2e `special_resource`, `gpu_stage` (mrp ABI diff); unit `TestEmitGPUSelectors` |
| `martian` module API (make_path, log_info/log_warn, update_progress, exit) | ✅ (real adapter drives it) | e2e `kitchen_sink`, `file_chain`, `api_smoke` |
| `martian.alarm` | ✅ each job writes every `_alarm` line to its task's stderr, where mrp prints `Alerts for <stage>:` as the stage completes. No run-level `Alerts:` summary on the Nextflow console | unit `TestAlarmsReachStderr` |
| `martian.get_memory_allocation` / `get_threads_allocation` (read `_jobinfo`) | ✅ | e2e `api_smoke` |
| directory-typed (`out path`) output published as a tree | ✅ | e2e `dir_out`, `dir_hidden` |
| stage output validation (mrp `verifyOutput`) | ✅ `mre post` checks a stage's final outs with Martian's `ValidateOutputs` and fails the task before any consumer binds them | e2e `TestInvalidOutputFailsLikeMrp`, `TestMrpDiffFailureParity` (`missing_struct_key`, `bad_out_type`); unit `TestPostRejectsInvalidOuts` |
| ASSERT vs retryable-error classification | ✅ `mre run` exits 42 for an `_assert` (terminate) vs 1 for any other failure; the config's `errorStrategy` routes accordingly | unit `TestOutcomes`; e2e `TestAssertTerminatesWithoutRetry`, `TestMrpDiffFailureParity` (`assert_min`, `assert_split`, `assert_join`, `stage_raise`) |
| auto-adjust-memory / OOM escalation | ✅ a retried task's memory is the stage's times the attempt; the stage keeps seeing its request | e2e `TestOrdinaryFailureRetriesWithEscalatedMemory` |
| thread-limit env vars (mrp's `thread_envs`) | ✅ every job runs with mrp's thread env vars set to its threads (`internal/job` `threadEnvs`; no test asserts the values) | — |
| `--monitor` memory enforcement | ✅ with `mro2nf -monitor`: `_jobinfo`'s `monitor_flag`, which `mrjob` enforces as under mrp | unit `TestMonitorFlag` |

## Outputs / storage — storage-management

| Feature | Status | Test |
|---|---|---|
| file outputs published to results (mrp `outs/` layout, `GetOutFilename` naming) | ✅ | e2e `file_min`, `TestMrpDiff` (tree diff vs real mrp) |
| inter-stage file passing | ✅ each task stages the bundles it reads, with their file leaves, as `path` inputs; markers locate the leaves | e2e `file_chain`, `TestCloudSim*`; unit `TestBundleRoundTrip` |
| file-array / struct-of-file / map-of-file outputs publishing | ✅ | e2e `map_file`, `map_file_keyed`, `struct_of_file`; unit `TestPublishOutsTreeLayout` |
| a file reached from several outputs | ✅ #431 named by the first path mrp published it at | e2e `shared_out_file` |
| VDR modes (disable/post/rolling/strict) timing | 🚫 no mid-run deletion | e2e `TestMrpDiffVDR/rolling`, `TestMrpDiffVDR/strict` (published outputs match mrp's under its VDR) |

## Running / inspecting — running-pipelines / inspecting-pipelines

| Feature | Status | Test |
|---|---|---|
| executor / jobmode (local + slurm/sge/lsf/pbs) | ✅ config profiles; the local one clamps a request to the host | e2e `TestLocalClampRunsOversizedRequest` |
| an ASSERT fails the run on its first attempt; a retried failure gets escalated memory | ✅ dynamic `errorStrategy { exitStatus==42 ? terminate : retry }` + `maxRetries 2` (which failures are retried: `retried-stage-errors`) | e2e `TestAssertTerminatesWithoutRetry`, `TestOrdinaryFailureRetriesWithEscalatedMemory` |
| isolated containers, AWS Batch, HealthOmics | ❌ not yet: the project runs on a local or shared filesystem (#489) | — |
| `mrp --overrides` (per-stage resource retune at launch) | ❌ not yet (#301); retune a stage with a `-c` config's `withName:` selector on its node alias. See `RUNTIME_TUNING.md` | — |
| `--maxjobs` / `--jobinterval` / `--localcores` / `--localmem` (throttling) | ✅ Nextflow `executor.queueSize` / `submitRateLimit` / `cpus` / `memory`; documented in `RUNTIME_TUNING.md` | — |
| `--profile` / `--inspect` / `--onfinish` (profile, dry-run, hook) | ✅ Nextflow `-with-trace`/`-with-report`, `-preview`, `workflow.onComplete`; see `RUNTIME_TUNING.md` | — |
| web UI / HTTP API / mrstat | 🚫 use `nextflow log`/`-with-report` instead | — |
| `mro check` / `mro graph` as conformance oracle | ✅ used to validate every fixture; `mro graph` ≈ `nextflow run -with-dag` | `test/e2e`, `mro check` |
| `.mro2nf.yml` flag defaults (keys mirror flags; builtin < config < explicit flag) | ✅ | unit `internal/config` `TestLoad*`; `cmd/mro2nf` `TestApplyConfig*` |

## Divergences from mrp

Every known divergence from `mrp` is an entry of the allowlist
[`test/e2e/divergences.json`](../test/e2e/divergences.json), and this table is
generated from it. The differential (`TestMrpDiff`, `TestMrpDiffFailureParity`,
the golden suites) excuses a difference only through an entry's matcher, so a
difference it sees that no entry matches fails a test, and removing an entry
fails the case it matched. Kinds: **abi** excuses a difference in a stage's `_args` or
`_outs`; **published-tree** one in the published `results/` tree or
`pipeline_outs.json`; **outcome** one in how a run ends; **mechanism** records a
difference in how mro2nf reaches `mrp`'s outputs that leaves them identical, so
it has no matcher and cites the tests that prove the outputs match. An entry
with an issue lasts until the issue is fixed: unless its matcher is an outcome,
which fails its case once mro2nf matches `mrp`, it is scoped to the fixtures
where it must excuse a difference, applies only there, and fails each of them
where it no longer does. A permanent entry is intentional and applies on every
fixture.

<!-- BEGIN GENERATED DIVERGENCES: `make docs-divergences` writes this table from test/e2e/divergences.json; edit that file, not the table. -->
| Feature | Status | Divergence | Test |
|---|---|---|---|
| file paths in the pipeline outs JSON | ⚠️ published-tree, permanent | **`outs-json-relative-paths`**: `pipeline_outs.json` names each published file by its path relative to `results/`, where mrp's `_outs` names it by its absolute path in the pipestance's `outs/`. `results/` may be a local dir, an S3 prefix or a HealthOmics output dir, so mro2nf writes paths that hold wherever the tree is published. The comparisons strip mrp's `outs/` prefix. | `TestMrpDiff/file_min`, `TestRemovingAPublishedTreeEntryFailsTheComparison`, `TestNormcmpAppliesTheAllowlist` |
| a published output that is a symlink | ⚠️ published-tree, permanent | **`published-symlinks-copied`**: Where mrp publishes a symlink in `outs/`, mro2nf publishes a copy of its target. For a stage output that was itself a symlink or lies outside the pipestance, mrp's `_outs` names the link target where `pipeline_outs.json` names the published copy. For a file that a second output also reaches, mrp links the second output's path to the file it already moved into `outs/`, and both outs JSONs name that first path. mrp links `outs/` to the target and reports the target (`martian/core/post_process.go` `moveOutFile`, `copyOutSymlink`). A publish dir may be an object store with no symlinks, and a host target is unreachable from an isolated worker, so mro2nf stages and publishes the content (#332, #431). The comparison reads an mrp-published symlink by its target's content, and maps a target outside `outs/` in mrp's `_outs` to the link's path; a target that several outputs reach, each through its own link, maps for each output to the link its `pipeline_outs.json` value names, each link for one output only. | `TestMrpDiff/symlink_escape`, `TestMrpDiff/shared_out_file`, `TestStageOutputLinkOutsideWorkCopiedLocally`, `TestRemovingAPublishedTreeEntryFailsTheComparison` |
| metadata files at the root of the published tree | ⚠️ published-tree, permanent | **`results-metadata-files`**: mro2nf publishes `pipeline_outs.json` and `manifest.json.gz` at the root of `results/`; mrp's `outs/` holds neither, since mrp writes its outs JSON to `<pipestance>/<PIPELINE>/fork0/_outs` and writes no manifest. A Nextflow run has no pipestance to hold the outs JSON, so it is published beside the outputs, with a machine-readable index of them (`manifest.json.gz`). The tree comparisons drop the two files at the root only, so a stage output of either name deeper in the tree still compares. | `TestMrpDiff`, `TestGolden`, `TestRemovingAPublishedTreeEntryFailsTheComparison` |
| a file below an array of two or more dimensions in a pipeline output | ⚠️ published-tree, permanent | **`unpublished-file-paths`**: Both runtimes leave such a file unpublished and name it where its task wrote it: mrp's `_outs` by its absolute path in the stage's directory of the pipestance, `pipeline_outs.json` by the absolute path its producing task recorded for it: in the Nextflow work dir, or in its scratch dir for a task that ran in one, as on an object-store work dir (#457). A file the stage never wrote keeps, in both, the path it was declared at. mrp descends an array of two or more dimensions one level as if its elements had the base type, so each element fails to move and is written as the stage wrote it, and mrp prints `Could not move output files` (`martian/core/post_process.go` `moveOutArrayDir`, `moveOutFile`, `moveOutDir`; `martian/core/node.go` `postProcess`). The two names cannot match: mrp's holds the pipestance's own chunk id (`chnk0-u<id>`) and mro2nf's the task's work hash, both new on every run. mre prints a warning for each such output. The comparison reads a path in mrp's `_outs` that lies in the pipestance outside `outs/`, and a path in `pipeline_outs.json` below a task's work dir, as one placeholder, `<unpublished>`. | `TestMrpDiff/nested_path_unpublished`, `TestPublishNDArrayLeavesFilesUnpublished`, `TestPublishLayoutNamesUnpublishedFileByItsTask`, `TestRemovingAPublishedTreeEntryFailsTheComparison` |
| failure messages and where a failure is recorded | ⚠️ outcome, permanent | **`failure-report-details`**: A failed run's messages differ: mre words its own errors and Nextflow adds context, and mro2nf records an output-validation failure in the task that ran the phase, where mrp records it at the fork. The failure contract is whether the run fails, which stages it blames and whether each failure is an ASSERT or an error, so the outcome comparison reads only those. | `TestMrpDiffFailureParity`, `TestOutcomeRulesNeedTheirEntries` |
| `--autoretry` / retrying a failed stage | ⚠️ outcome, #299 | **`retried-stage-errors`**: mro2nf retries every stage failure except an ASSERT up to twice (`errorStrategy` retry, `maxRetries 2`), where mrp retries only failures matching its `retry_on` patterns (signals, ENOMEM, scheduler errors) and never an ordinary stage exception. A stage that fails and then succeeds on retry completes under mro2nf and fails under mrp (mrp's `jobmanagers/retry.json`; `internal/cgnf/config.go` `errorStrategy`). The outcome comparison counts a retried task's failures once on the failure fixtures whose error-failing task Nextflow retries (a stage, or the OUTS task on `map_pipe_nested`), and fails on any of them where no task is retried. Scoped to `bad_out_type`, `entry_dir`, `entry_file`, `entry_file_empty`, `entry_filearr`, `entry_mapfile`, `entry_struct_file`, `map_pipe_nested`, `missing_struct_key`, `split_bad_mem`, `split_bad_special`, `split_bad_threads`, `split_from_file`, `stage_raise`. | `TestMrpDiffFailureParity/stage_raise`, `TestOutcomeRulesNeedTheirEntries` |
| `(local)` | ⚠️ mechanism, permanent | **`local-modifier-ignored`**: mro2nf ignores `local` and schedules the stage on the configured executor like any other task; in a cluster jobmode mrp runs it through its local job manager on the head node, sized against the head node's capacity. Scheduling only: the stage runs the same code on the same `_args`. Under `--jobmode=local`, which the differential uses, the local job manager is mrp's only one (`martian/core/node.go`, `stage.go`), so `kitchen_sink`'s local SUM_SQUARES matches mrp; cluster jobmodes are not compared with mrp. | `TestMrpDiff/kitchen_sink` |
| `outs/` move+symlink layout | ⚠️ mechanism, permanent | **`outs-files-copied`**: mrp moves each output file into `outs/` and leaves a symlink to it in the stage's files dir; mro2nf copies each output into `results/` and leaves the task's work dir as it was. Both publish regular files with the same paths and bytes: the tree comparison, which tells a symlink from a file, proves it on every `TestMrpDiff` fixture. Where mrp publishes a symlink instead, `published-symlinks-copied` applies. | `TestMrpDiff/file_min`, `TestMrpDiff/dir_out`, `TestMrpDiff/map_file` |
| `(preflight)` | ⚠️ mechanism, #314 | **`preflight-dag-order`**: A preflight runs in DAG order, where mrp makes every other call in its pipeline wait for it, so a failing preflight stops that pipeline before they start. A preflight has no outputs (mrp rejects one with `PreflightOutputError`), so the order changes no output: `preflight_ungated`'s outputs and stage invocations match mrp's. On a failing preflight, mrp has started no other call of its pipeline, while mro2nf may have. | `TestMrpDiff/preflight_ungated` |
| `-resume` ≈ restart-without-rerun | ⚠️ mechanism, permanent | **`resume-content-cache`**: mro2nf resumes through Nextflow's `-resume`, which reuses each task whose script and inputs hash the same, where mrp restarts a pipestance from the stages it recorded as complete. A cache hit replays the task's recorded outputs: an unchanged rerun re-executes no task and leaves the published tree and `pipeline_outs.json` unchanged, and a fresh run matches mrp. Every script names the mro2nf build, so a project re-transpiled by another build re-runs every task. A rerun after a failure is not compared with mrp's restart. | `TestResumeCachesEverything`, `TestFreshRunsAreIdentical`, `TestResumeKeysOnTheBuild`, `TestMrpDiff` |
| stage / pipeline `retain` | ⚠️ mechanism, permanent | **`retain-no-vdr`**: `retain` has nothing to do under mro2nf, which deletes no file, so every file is kept. `retain` only exempts a file from VDR. `vdr_volatile` retains a volatile stage's file and publishes what mro2nf does under mrp's rolling and strict VDR. | `TestMrpDiffVDR/rolling`, `TestMrpDiffVDR/strict` |
| `(volatile)` / `volatile = strict` | ⚠️ mechanism, permanent | **`volatile-no-vdr`**: mro2nf ignores `volatile` and deletes no file mid-run, so `work/` keeps every intermediate file mrp's volatile data removal (VDR) deletes once its consumers finish (higher peak disk). VDR deletes only files no later stage reads and never a published output: `vdr_volatile` publishes what mro2nf does under mrp's rolling and strict VDR, while mrp deletes the volatile file its consumer has read. | `TestMrpDiffVDR/rolling`, `TestMrpDiffVDR/strict`, `TestMrpDiff/kitchen_sink` |
<!-- END GENERATED DIVERGENCES -->

## Notes

- Every e2e fixture's expected output is the real `mrp` result, captured under
  `testdata/<name>/expected/`. A fixture `mrp` fails on is a failure-parity case
  (`TestMrpDiffFailureParity`) and has no golden.
- The call graph, its forks and every stage's `_args` are Martian's own: `mre`
  compiles the run's invocation with Martian's compiler and resolves each
  instance with Martian's binding, projection and type functions, mirroring
  only the per-instance resolution, fork expansion and disable test Martian
  does not export (`docs/TRANSPILER.md` §6).
- Former `mrp`-runtime-only items:
  - **content-based retry (ASSERT vs retryable)** — `mre run` exits 42 for an
    `_assert` and 1 otherwise; the config's dynamic `errorStrategy` terminates
    the former and retries the latter, where `mrp` retries only transient
    failures (`retried-stage-errors`, #299).
  - **`--monitor` memory enforcement** — `mro2nf -monitor` sets `_jobinfo`'s
    `monitor_flag`, which `mrjob` enforces as under `mrp`.
  - **VDR rolling/strict mid-run deletion** — not implemented: the whole `work/`
    tree is retained (higher peak disk, identical outputs).
  - **live UI / HTTP API** — no faithful Nextflow analog.
- Scheduler-*allocation* fidelity: per-chunk `cpus`/`memory`, the split-returned
  `join` override, and `special` → `clusterOptions` all reach the scheduler, so
  a transpiled run provisions like mrp and runs in a comparable duration, while
  every job reports mrp's resolved request in `_jobinfo`.
- The published `outs/` tree reproduces mrp's nested layout and
  `GetOutFilename` naming (verified by the Go `TestMrpDiff` suite against real
  mrp). Every remaining ⚠️ item is a row of
  [Divergences from mrp](#divergences-from-mrp).
- **Embedded absolute paths (shared-filesystem assumption).** The tasks stage
  *declared* file leaves; a path a stage bakes into another file's *content*
  (e.g. a JSON manifest that names other files by absolute path) is invisible
  to the type walk and cannot be relocated. On a shared filesystem it resolves,
  as under `mrp`; an isolated container downstream would not see it (#489).
