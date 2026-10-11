# Martian feature coverage

This matrix tracks every Martian feature (from the martian-lang.org docs and the
authoritative `martian/syntax` grammar) against the transpiler's support and the
test that exercises it. Test types: **e2e** = transpile + run under Nextflow,
output diffed against real `mrp` (the Go `test/e2e` suite); **unit** = Go test;
**corpus** = parse+lower robustness sweep.

Provenance: this matrix was built from a systematic page-by-page audit of all
seven martian-lang.org docs (writing-pipelines, language-details, writing-stages,
advanced-features, storage-management, running-pipelines, inspecting-pipelines)
cross-checked against the `martian/syntax` grammar; each documented feature was
classified covered/partial/guarded/out-of-scope and gaps were closed with the
fixtures cited below. Generated output targets the latest Nextflow (verified on
26.04.x), DSL2 (the modern default — no `nextflow.enable.dsl` needed), and runs
warning-free (no deprecated/`useless` operators).

Status: ✅ supported & tested · ⚠️ supported with a divergence from `mrp`, every
one a row of [Divergences from mrp](#divergences-from-mrp), generated from the
allowlist the differential applies · ❌ unsupported, guarded with a clear error &
tracked · 🚫 runtime/`mrp`-only, out of scope for a transpiler.

**How unsupported features are signalled (never silent):**
- **❌ hard error** — the transpile fails (`UnsupportedError` / parse error,
  non-zero exit): an unknown expression (e.g. `sweep`), an unknown adapter
  language, a `comp` stage without `mrjob`, a nested typed-map projection
  (`map<map<S>>.field`), an invalid `-target`. Lowering errors by default on any
  unrecognized expression, so a construct cannot be silently dropped.
- **⚠️ warning** — the project transpiles but a no-op divergence is **logged**
  by `mro2nf`: a `local` or `volatile` call modifier (no VDR; a stage's
  `volatile = strict` is not logged, #342), and a mapped or disabled
  `preflight` (it runs in DAG order rather than gating early).
- **🚫 / layout** — documented here with no output-observable impact.

## Language — writing-pipelines / language-details

| Feature | Status | Test |
|---|---|---|
| stage / pipeline / top-level call | ✅ | e2e `split_test`, all |
| top-level call targeting a bare **stage** | ✅ | e2e `stage_entry` |
| entry inputs as run parameters (override at launch) | ✅ each entry input → `params.<name>` (defaults to the baked `.mro` value); BUILD_ENTRY_ARGS (`mre entryargs`) overlays a `-params-file` / HealthOmics run params on the defaults; CLI `--<name>` strings are parsed into the declared scalar int/float/bool, and every override float takes `mrp`'s literal encoding (`2.0` → `2`) | unit `TestEmitEntryParams`, `TestTypeOverrides`; e2e `TestCLITypedOverrides`; verified default + override |
| file-typed entry inputs (FASTQ etc.) overridable at launch | ✅ **every shape** — scalar `file`/`path`/dir, `file[]`, `map<file>`, and struct-with-file. The value's file leaves are flattened to a list (canonical type-walk order: arrays in index order, maps by sorted key, struct fields in order), `file()`'d on the head node and staged as one `path` input, so an isolated worker reads real localized files; `mre entryargs -fileflat` pops the staged paths back into the value and marks them into the bundle. An S3 URI / `s3://` prefix (directory) is staged by Nextflow / HealthOmics. A value with no file leaf (`[]`, `{}`, a null file field) stages zero files. Each leaf is vetted on the head node before `file()`: an `s3://` URI on every target, a local path only on `local`, any other scheme only with `--mro2nf_allow_remote_inputs true`. A path holding `*?[]{}$` or a control character fails loudly, because Nextflow's stage-in script leaves them for bash to expand (`TRANSPILER.md` §8) | e2e `TestEntryFileRemoteURL`, `TestLibEntryFile`, `TestEntryFileGlobCharName`; `entry_file`/`entry_filearr`/`entry_mapfile`/`entry_struct_file`/`entry_file_empty` (+ `_override`); docker e2e `TestDockerEntryOverrides` with inputs **outside the image** (arrive only via staging); live Batch + HealthOmics; unit `TestEmitEntryFileParam`, `TestEmitEntryFileArray`, `TestEmitEntryStructFile`, `TestReconstructFiles`, `TestCopyTreeDerefSymlink` |
| pipeline with **no calls** (return-only) | ✅ | e2e `returnonly` |
| `@include` (resolved via MROPATH) | ✅ | e2e `include_test` |
| `@include` diamond / cycle / missing | ✅ (parser-enforced) | via `martian/syntax`; corpus parse |
| `filetype` | ✅ | e2e `file_min`, `kitchen_sink` |
| `struct` types | ✅ | e2e `struct_min`, `struct_proj`, corpus |
| named / unnamed (`default`) out params | ✅ | e2e `default_out` |
| out filename (`out txt r "" "f.txt"`) | ✅ | e2e `file_min` |
| scalar types int/float/string/bool/path/file/map | ✅ | e2e (various) |
| typed arrays `T[]`, multi-dim `T[][]` | ✅ | e2e `multidim`, `split_test` |
| typed maps `map<T>` (output) | ✅ | e2e `typedmap_out` |
| int→float coercion; float→int rejected | ✅ | `martian/syntax` (corpus parse) |
| whole-number float literal → int param (`5.0` → `5`) | ✅ (type-directed `CoerceScalars` on bound inputs only; a split-returned chunk arg reaches main and join as the split wrote it, as mrp's `ChunkDef.Merge` passes it through) | e2e `float_to_int`, `chunk_float_int`; unit `TestCoerceScalars`, `TestProducerCoerceInputs` |
| literals: int/float/sci, bool, null, arrays, maps, struct literals, escapes | ✅ | `martian/syntax`; e2e entry args |
| bindings: `self.x`, `self.x.y`, `STAGE`, `STAGE.out`, `STAGE.out.field`, `STAGE.default` | ✅ | unit `bind`; e2e `struct_proj`, `default_out` |
| **field projection through arrays** (`CALL.s.field`) | ✅ | unit `TestResolveArrayProjection`, e2e `struct_proj` |
| field projection through maps (`map<S>.field`) | ✅ (emitter computes a per-ref `MapDepth` from the program types; the binder projects the field over the map's values — covers declared `map<S>` outputs and map-fork-induced maps) | e2e `map_struct_proj`, unit `TestResolveMapProjection`, `TestMapProjectDepth` |
| field projection through an **array of typed maps** (`array<map<S>>.field`) | ✅ the binder descends the array and projects the field over each map's values, preserving the array shape (`-> array<map<field>>`); the emitter marks it with `Ref.MapInArray` | unit `TestResolveArrayOfMapProjection`, `TestMapProjectDepth`, `TestCheckSupported` |
| field projection through **nested typed maps** (`map<map<S>>.field`) | ❌ rejected at emit time (`checkSupported` → `ErrUnsupported`) — one projection level cannot faithfully lower nested maps, so it errors rather than mis-projecting | unit `TestCheckSupported`, `TestMapProjectDepth` (depth −1) |
| literal edge cases: negative numbers, `>2^53` int64 precision, unicode/escape strings | ✅ | e2e `literals_edge` |
| wildcard binding `* = self` / `* = REF` | ✅ | e2e `wildcard` (compiler expands) |
| **refs nested in array/map literals** (`[A.x, B.x]` fan-in) | ✅ #14 | unit `bind`/`fork`, e2e `fanin` |
| struct duck typing (extra fields) | ✅ every bound value is narrowed to the receiving param's declared struct, as mrp's FilterJson (extra fields and their file leaves dropped, a missing field null), in stage `_args` and pipeline outputs; a forward of any struct-typed value keeps its BIND so the narrowing runs, even when the types match | e2e `struct_narrow`; unit `TestNarrow`, `TestRunBindNarrowsStructs`, `TestRunForkBindNarrowsStructs` |
| struct and map key order | ✅ #366 a value reaches a stage's `_args` and `pipeline_outs.json` in mrp's key order: a projection of a producer's output keeps the producer's order unless it is narrowed, which rebuilds it in declared order; a struct, array or typed map built from bindings, a literal, a whole-call ref (one struct per fork for a mapped call) and the file-bearing structs of the outs JSON have their keys sorted; an untyped map and a split's chunk args keep the order they were written in | e2e `struct_key_order`, `nested_struct`, `kitchen_sink`; unit `TestNarrowKeyOrderMatchesMrp`, `TestRunBindKeepsKeyOrder`, `TestRunPublishLayoutKeepsKeyOrder` |
| null inputs / null propagation | ✅ | e2e `null_in`, `modifiers_min` |

## Calls & modifiers

| Feature | Status | Test |
|---|---|---|
| aliased calls `call X as Y` (repeated) | ✅ | e2e `alias_min`, `kitchen_sink` |
| nested pipeline calls (pipeline→pipeline) | ✅ | e2e `modifiers_min`, `kitchen_sink` |
| non-linear / diamond DAGs | ✅ | e2e `diamond_min` |
| `using (disabled = ref)` (self or call ref) | ✅ A gate that is null at run time (a stage output, or a sub-pipeline input bound to one) leaves the call enabled, as mrp's `Fork.disabled` decodes null into a false bool; a projection through a null struct fails, as in mrp. A gate null before any stage runs (an invocation value, a literal bound into a sub-pipeline input, an output of a statically disabled call) fails the transpile with mrp's `disabled cannot be bound to a null value` binding error; a gate on an output of a call that is gated at run time fails it with mrp's `disabled was bound to value ... which would be disabled at runtime` binding error | e2e `modifiers_min`, `null_gate`, `TestLibHelpers`; unit `TestCheckCallGraphStaticNullGate` |
| whole-call ref (`s = CALL`) to a **disabled** stage or pipeline call | ✅ null, as mrp (the skip bundle is a whole JSON null; a disabled sub referenced whole keeps its boundary rather than inlining) | e2e `disabled_wholeref`; unit `TestEmitDisabledNullBundleIsNull`, `TestInlineDisabledWholeRefKeepsBoundary` |
| `(preflight)` | ✅ gates the pipeline (early-abort): an input-bound preflight runs first and the pipeline's `pa` is gated on its completion, so every downstream call waits — mrp's prenode behavior. A mapped or disabled preflight stays in DAG order (warns; `preflight-dag-order`) | e2e `modifiers_min`, `kitchen_sink`; unit `TestEmitPreflightGate` |

## Map calls / forks — writing-stages / advanced-features

| Feature | Status | Test |
|---|---|---|
| `map call ... split` over an **array** → array result | ✅ | e2e `fork_min` |
| `map call ... split` over a **map** → keyed result | ✅ | e2e `map_fork`, unit `TestResolveForksMap/Merge` |
| fused multi-`split` (zipped) | ✅ | e2e `multisplit` |
| whole-call ref (`x = CALL`) to a **map call** → one struct per fork (array of structs / map of structs, as mrp's resolveMerge) | ✅ (the bindspec ref carries the fork kind; the binder regroups the merged per-output collections per fork). A whole ref to a mapped call with no outputs is a transpile error | e2e `map_wholeref`; unit `TestResolveWholeRefToMappedCall`, `TestValueToEntryMarksWholeMappedRef` |
| empty split collection → null result | ✅ | e2e `empty_fork_min` |
| map call over a **split stage** (fork-key threaded) | ✅ #16 | e2e `map_split` |
| map call over a **sub-pipeline** (per-fork body keying) | ✅ #16 | e2e `map_pipe`, `map_pipe_split` |
| map call emitting a **file** (array/keyed-map of files) | ✅ | e2e `map_file`, `map_file_keyed` |
| **complex combo**: map over a sub-pipeline whose body is a split stage emitting a file (Cell Ranger per-sample pattern) | ✅ | e2e `map_split_file` (also under `TestDockerIsolation`) |
| **complex combo**: py + exec + comp adapters chained in one pipeline | ✅ | e2e `mixed_adapters` |
| **complex combo**: struct output whose field is a file array (struct walk → file[]) | ✅ | e2e `struct_file_array` (also under `TestDockerIsolation`) |
| empty-split fork inside a map call (0 chunks → 0/null, no fork dropped) | ✅ | e2e `map_split` (includes an empty fork) |
| `disabled` on a map call | ✅ (fork pipeline-args gated by the resolved flag; skip → null bundle) | e2e `disabled_map` (skip true→null, false→[2,4,6]) |
| map call over a pipeline with an internal **disabled** call | ✅ (keyed pipeline gates the call per fork) | e2e `map_pipe_disabled` |
| **nested** map call (a map over a pipeline that itself maps) | ✅ (2-D fork keying: composite `outer~inner` keys, keyed FORKBIND/MERGE; the keyed FORKBIND enumerates per-fork bundle dirs from a staged `forknames.json` read via `.resolve()`, which is object-store-safe — `java.io.File.listFiles()` is **not** used) | e2e `map_pipe_nested`; live Batch + S3 (`map_pipe_nested`); unit guard `TestEmitDisabledNestedMap` (forbids `listFiles()`) |
| **disabled** nested map call (a disabled map nested inside an outer map) | ✅ (keyed disable gate: the flag is resolved per outer fork and either runs that fork's inner map or emits its null bundle) | e2e `map_pipe_disabled_nested` (fork0→[2,4], fork1 skip→null) |

## Stages / adapters / resources

| Feature | Status | Test |
|---|---|---|
| split / main / join | ✅ | e2e `split_test`, `kitchen_sink` |
| ordered chunk-outs aggregation in join | ✅ | shim `TestRunSumSquares`; mre `TestReadChunkData`, `TestOrderByIndexMatchesSortV` |
| `py` adapter | ✅ | e2e (all py) |
| `exec` adapter | ✅ | e2e `exec_min` |
| `comp` adapter (mrjob-wrapped) | ✅ | unit `TestRunSumSquaresComp` |
| per-chunk resources `__mem_gb/__threads/__vmem_gb/__special` | ✅ in `_args`/`_jobinfo`; a value of the wrong JSON type (a non-number `__mem_gb`, a non-string `__special`) fails the split with an error, as under mrp (#275) | unit `TestMergeArgs*`, `TestSpecialResourcePreserved`, `TestJobInfoResolvedResources`, `TestReadStageDefsBadResourceValue`; e2e `TestMrpDiffFailureParity` (`split_bad_special`, `split_bad_mem`, `split_bad_threads`) |
| per-chunk resources → Nextflow scheduler directives | ✅ #15 (dynamic `cpus`/`memory` closures read the chunk's resolved resources) | e2e `split_test`, unit |
| resources a stage sees (`__*`, `_jobinfo`) | ✅ #243 every phase passes the stage's Martian request to the shim (not the integer, attempt-scaled `task.memory`), which overlays the chunk's or the staged `joinres.json`'s own request and resolves it as mrp's local job manager does: fractional memory kept to the MiB, mrp's `__*` number format | e2e `frac_mem` (golden + mrp ABI diff), unit `TestResourceFlagsPassStageMemory`, `TestResolveResourcesOverlay`, `TestResourceNumMatchesMrp` |
| negative/adaptive resource sentinels | ✅ preserved | unit `TestMergeArgsNegativeResources` |
| stage `using(mem_gb/threads)` incl. fractional → cpus/memory | ✅ the scheduler gets whole units; the stage sees the fractional request (#370: `threads = 0.5` reaches `__threads` as 0.5) | unit `TestEmitModules` (`memory '2 GB'`, `cpus 1`), `TestResourceFlagsPassStageThreads`; e2e `frac_threads`, `split_test` (mrp ABI diff) |
| `using(vmem_gb)` enforcement | ✅ the declared value is passed to the shim via `-vmemgb` on every phase, so `mro2nf -monitor` caps the adapter's virtual memory (`prlimit --as`) at it and `_jobinfo` reports it; a per-chunk `__vmem_gb` overrides for main, a split-returned join `__vmem_gb` for join | unit `TestEmitVmemFlag`, `TestLimitedCommandMonitor` |
| `using(special)` → scheduler | ✅ mapped to a `clusterOptions` directive on every phase, looked up from `params.job_resources` (the `MRO_JOBRESOURCES` analog: a `special` string keys into a deploy-supplied map of scheduler options). Default map is empty (no-op on local; populate per deployment). A per-task `__special` (per-chunk / split-returned join) wins over the static key. The static key also reaches main and join `_args` as `__special` (`-special`), as mrp writes it | e2e `special_resource`, `gpu_stage` (mrp ABI diff), unit `TestResourceFlagsPassStaticSpecial`, unit `TestEmitSpecialScheduler`, `TestSpecialResourcePreserved` |
| split-returned `join` resource override → join phase directives | ✅ the split's `{"join": {...}}` block (`readStageDefs`) is emitted as `joinres.json` and read by the JOIN process's dynamic `cpus`/`memory`/`clusterOptions`, overlaying the stage's static `using()` field-by-field — matching mrp's `getJobReqs`. JOIN provisions and reports the overridden allocation | e2e `join_resources`, unit `TestReadStageDefsJoinOverride`, `TestEmitJoinResourceOverride` |
| `martian` module API (make_path, log_info/log_warn, update_progress, exit, alarm) | ✅ (real adapter drives it) | e2e `kitchen_sink`, `file_chain`, `api_smoke` |
| `martian.get_memory_allocation` / `get_threads_allocation` (read `_jobinfo`) | ✅ | e2e `api_smoke` (using(mem_gb=3,threads=2)→mem 3,threads 2) |
| directory-typed (`out path`) output published as a tree | ✅ | e2e `dir_out` (CopyTree dir recursion) |
| stage output validation (mrp `verifyOutput`) | ✅ a stage's final `_outs` (non-split main, or join) are checked like mrp: every declared output present, each non-null value of its declared type (a struct with every member key, an int a JSON integer, a typed map of files with filename-legal keys); a violation fails the stage task before any consumer binds it. Per-chunk outs are not checked, as at mrp's default enforcement level | e2e `TestInvalidOutputFailsLikeMrp` (`missing_struct_key`), `TestMrpDiffFailureParity` (`missing_struct_key`, `bad_out_type` fail the same stage as under real mrp); unit `TestValidateOutputs`, `TestProducerValidateOuts` |
| ASSERT vs retryable-error classification | ✅ shim exits 42 for an ASSERT (terminate) vs 1 for a retryable failure; config's dynamic `errorStrategy` routes accordingly | unit `TestStageFailureClassification`, `TestEmitConfig`; e2e `TestMrpDiffFailureParity` (`assert_min`, `assert_split`, `assert_join` are an ASSERT and `stage_raise` an error under real mrp and mro2nf alike) |
| auto-adjust-memory / OOM escalation | ✅ memory directives grow with `task.attempt`, so an OOM-killed stage (a retryable failure) is retried with more memory instead of failing identically; cpus do not escalate. Attempt 1 is unchanged. The escalation sizes the task and the `-monitor` limits only; the stage keeps seeing its request | unit `TestEmitModules` (`* task.attempt`), `TestMonitorLimitsFollowRetryAllocation`; e2e `TestOrdinaryFailureRetriesWithEscalatedMemory` (`flaky_retry`) |
| thread-limit env vars (mrp's `thread_envs`) | ✅ #247 every stage runs with `GOMAXPROCS`, `MKL_NUM_THREADS`, `NUMEXPR_MAX_THREADS`, `NUMEXPR_NUM_THREADS`, `OMP_NUM_THREADS`, `OMP_THREAD_LIMIT`, `POLARS_MAX_THREADS`, `ROOT_MAX_THREADS` and `TF_NUM_THREADS` set to ceil(threads), overriding inherited values as mrp does | unit `TestAdapterThreadEnvs` |
| `--monitor` memory enforcement | ✅ with `mro2nf -monitor`: an RSS kill at `mem_gb` via the process-group monitor (mrp's primary kill) plus a `prlimit --as` vmem cap at `vmem_gb` | unit `TestMemMonitor*`, `TestRunAdapterMonitorMemoryQuota`, `TestLimitedCommandMonitor` |

## Outputs / storage — storage-management

| Feature | Status | Test |
|---|---|---|
| file outputs published to results (mrp `outs/` layout, `GetOutFilename` naming) | ✅ | e2e `file_min`, `TestMrpDiff` (tree diff vs real mrp) |
| inter-stage file passing (shared FS) | ✅ | e2e `file_chain` |
| inter-stage file passing (object store, no shared FS) | ✅ #13 (de-bundled channels: each payload is a typed sidecar + individual leaf `path` items, referenced via `@mre:file:`/`@mre:dir:` markers; the shim absolutizes on input, relativizes on output) | e2e `TestCloudSim*`, `TestDockerIsolation`, unit `TestBundleRoundTrip` |
| auxiliary files reach isolated workers (types.json, bindspecs) | ✅ each task stages only the files it needs — the shared `types.json` plus, for a bind/fork process, its own `spec.json` — as individual `path` inputs, never referenced by `${projectDir}` (invisible on an AWS Batch / HealthOmics worker, which mounts only its work dir). A task transfers only its own bindspec, not every call's | e2e `TestDockerIsolation` (container that does not mount the project dir), unit `TestEmitAssetsStaged` |
| file-array / struct-of-file / map-of-file outputs publishing | ✅ #13 (PUBLISH walks the entry's output type, descending file-bearing structs) | e2e `map_file`, `map_file_keyed`, `struct_of_file`; unit `internal/types` |
| VDR modes (disable/post/rolling/strict) timing | 🚫 no native dependency-gated mid-run deletion | e2e `TestMrpDiffVDR/rolling`, `TestMrpDiffVDR/strict` (published outputs match mrp's under its VDR) |

## Running / inspecting — running-pipelines / inspecting-pipelines

| Feature | Status | Test |
|---|---|---|
| executor / jobmode (local + slurm/sge/lsf/pbs) | ✅ config profiles | unit `TestEmitConfig` |
| an ASSERT fails the run on its first attempt; a retried failure gets escalated memory | ✅ dynamic `errorStrategy { exitStatus==42 ? terminate : retry }` + `maxRetries 2`; the shim's ASSERT exit code drives it (which failures are retried: `retried-stage-errors`) | unit `TestEmitConfig`, `TestStageFailureClassification`; e2e `TestAssertTerminatesWithoutRetry`, `TestOrdinaryFailureRetriesWithEscalatedMemory` |
| cloud executors (awsbatch/k8s) | ✅ profiles emitted; #13 bundle data plane makes file flow object-store-correct; auxiliary files staged via `_assets` so isolated workers (no shared FS) can read them | config, e2e `TestCloudSim*` (copy-staging), `TestDockerIsolation` (true container isolation) |
| `-target awsbatch` (AWS Batch + S3) | ✅ Batch executor + classic aws-CLI S3 staging, in-container `/opt/mro2nf` paths, a generated `Dockerfile` + self-contained `runtime/` build context (bash/ps, no ENTRYPOINT, x86_64, aws CLI) | unit `TestEmitConfigTargets`, `TestEmitContainerBuild`; e2e `TestGeneratedAWSBatchImage` (built from the generated Dockerfile) |
| `-target healthomics` (AWS HealthOmics) | ✅ ECR-parameterized container, publishes to `/mnt/workflow/pubdir`, no executor (managed), pinned Nextflow version, `parameter-template.json` + `package.sh` (workflow zip) | unit `TestEmitConfigTargets`, `TestEmitHealthOmicsPackaging` |
| `mrp --overrides` (per-stage resource retune at launch) | ✅ `mro2nf overrides` converts the JSON to a `process`/`withName:` `-c` overlay; or write it natively. See `RUNTIME_TUNING.md` | unit `TestConvert` |
| `--maxjobs` / `--jobinterval` / `--localcores` / `--localmem` (throttling) | ✅ Nextflow `executor.queueSize` / `submitRateLimit` / `cpus` / `memory`; documented in `RUNTIME_TUNING.md` | — |
| `--profile` / `--inspect` / `--onfinish` (profile, dry-run, hook) | ✅ Nextflow `-with-trace`/`-with-report`, `-preview`, `workflow.onComplete`; see `RUNTIME_TUNING.md` | — |
| web UI / HTTP API / mrstat | 🚫 use `nextflow log`/`-with-report` instead | — |
| `mro check` / `mro graph` as conformance oracle | ✅ used to validate every fixture; `mro graph` ≈ `nextflow run -with-dag` | `test/e2e`, `mro check` |

## Opt-in emission mode — `-native`

The flag changes the emitted *task graph*, never the outputs, so each gate
compares its run against the committed real-`mrp` golden, and `TestMrpDiff`
anchors those goldens to live `mrp`: its anchor fixtures additionally run a
`-native` Nextflow leg against the same mrp pipestance, so the native emit
path faces real `mrp` output, not only committed goldens. The trade-offs the
flag opts into are surfaced as transpile-time diagnostics
(`internal/emit/diagnostics.go`) — see `docs/TRANSPILER.md` §4.8.

| Shape under the flag | Status | Test |
|---|---|---|
| `-native` full collapse (plain/diamond DAGs, linear chains, value-only array & map forks, upstream split sources, invocation-known & runtime empty forks, file outputs through the native LAYOUT, wildcard/alias, exec+comp adapters) | ✅ **no** data-plane task remains (`assertNativeComplete`: no BIND/FORK/MERGE/DISABLE/BUILD_ENTRY_ARGS) and output equals the mrp golden | e2e `TestNativeComplete` |
| `-native` bounded remainder (file-bearing / multi-split / split-stage forks keep exactly one FORK resolve; map-over-sub-pipeline runs the keyed layer; a disabled map keeps FORK+MERGE as the skip mix point; nested value-only maps ride the `_KS` driver scatter) | ✅ the exact surviving data-plane process names are pinned per fixture, and each prints an Info diagnostic at transpile | e2e `TestNativeMode` |
| `-native` scatter/merge-fold specifics (disabled sub-pipeline callees, fan-out consumers keeping their MERGE, empty-fork cascades) | ✅ | e2e `TestNativeScatter` |
| `-native` file- and directory-typed entry inputs (baked leaves staged into the workflow) | ✅ | e2e `TestNativeFileEntry` |
| `-native` launch-time override of a baked entry arg | ❌ **by design**: loud error, never a silent ignore — re-transpile to change entry values (HealthOmics: `parameter-template.json` still declares the params; supplying one fails the run, per the transpile-time Info) | e2e `TestNativeRejectsOverride`; unit `TestDiagnoseNativeHealthOmics` |
| `-native` on container backends (baked entry args in the image) | ✅ | e2e `TestNativeContainerEmits`; docker e2e `TestGeneratedAWSBatchImageNative` |
| `-native` plan eligibility (what scatters, what keeps the FORK path, merge-fold rules, queue-pipeargs contexts) | ✅ decided once in `buildPlan`, so emitters cannot disagree | unit `internal/emit/native_test.go` (`TestBuildPlanNativeScatter*`, `TestBuildPlanMergeFold`, `TestSplitValueOnly`, …) |
| every fixture statically linted with and without `-native` | ✅ a Groovy syntax error in any `-native`-only emission branch fails the lint gate | e2e `TestNextflowLint` |
| `-native` orchestration stays flat across fork widths / chunk counts | ✅ the bench gate's `-native` lanes require identical plumbing-task counts at two widths (constant OK, scaling is a bug) | e2e `TestBench` (`bench/forks`, `bench/split` native lanes; gated in `bench/baseline.json`) |
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
| a published output that is a symlink | ⚠️ published-tree, permanent | **`published-symlinks-copied`**: Where mrp publishes a symlink in `outs/`, mro2nf publishes a copy of its target. For a stage output that was itself a symlink or lies outside the pipestance, mrp's `_outs` names the link target where `pipeline_outs.json` names the published copy. For a file that a second output also reaches, mrp links the second output's path to the file it already moved into `outs/`, and both outs JSONs name that first path. mrp links `outs/` to the target and reports the target (`martian/core/post_process.go` `moveOutFile`, `copyOutSymlink`). A publish dir may be an object store with no symlinks, and a host target is unreachable from an isolated worker, so mro2nf stages and publishes the content (#332, #431). The comparison reads an mrp-published symlink by its target's content, and maps a target outside `outs/` in mrp's `_outs` to the link's path. | `TestMrpDiff/symlink_escape`, `TestMrpDiff/shared_out_file`, `TestStageOutputLinkOutsideWorkCopiedLocally`, `TestRemovingAPublishedTreeEntryFailsTheComparison` |
| metadata files at the root of the published tree | ⚠️ published-tree, permanent | **`results-metadata-files`**: mro2nf publishes `pipeline_outs.json` and `manifest.json.gz` at the root of `results/`; mrp's `outs/` holds neither, since mrp writes its outs JSON to `<pipestance>/<PIPELINE>/fork0/_outs` and writes no manifest. A Nextflow run has no pipestance to hold the outs JSON, so it is published beside the outputs, with a machine-readable index of them (`manifest.json.gz`). The tree comparisons drop the two files at the root only, so a stage output of either name deeper in the tree still compares. | `TestMrpDiff`, `TestGolden`, `TestRemovingAPublishedTreeEntryFailsTheComparison` |
| a file below an array of two or more dimensions in a pipeline output | ⚠️ published-tree, permanent | **`unpublished-files-null`**: mrp leaves such a file unpublished, and its `_outs` names the file's absolute path in the stage's directory of the pipestance. mro2nf leaves it unpublished too, and `pipeline_outs.json` names it null. mrp descends an array of two or more dimensions one level as if its elements had the base type, so each element fails to move and is written as the stage wrote it, and mrp prints `Could not move output files` (`martian/core/post_process.go` `moveOutArrayDir`, `moveOutFile`, `moveOutDir`; `martian/core/node.go` `postProcess`). That path holds the pipestance's own chunk id (`chnk0-u<id>`), so no other run reproduces it, and `pipeline_outs.json` names files relative to `results/`, where an unpublished file has no path. mre prints a warning for each such output. The comparison reads a path in mrp's `_outs` that lies in the pipestance outside `outs/` as null. | `TestMrpDiff/nested_path_unpublished`, `TestPublishNDArrayLeavesFilesUnpublished`, `TestRemovingAPublishedTreeEntryFailsTheComparison` |
| failure messages and where a failure is recorded | ⚠️ outcome, permanent | **`failure-report-details`**: A failed run's messages differ: mre words its own errors and Nextflow adds context, and mro2nf records an output-validation failure in the task that ran the phase, where mrp records it at the fork. The failure contract is whether the run fails, which stages it blames and whether each failure is an ASSERT or an error, so the outcome comparison reads only those. | `TestMrpDiffFailureParity`, `TestOutcomeRulesNeedTheirEntries` |
| `--autoretry` / retrying a failed stage | ⚠️ outcome, #299 | **`retried-stage-errors`**: mro2nf retries every stage failure except an ASSERT up to twice (`errorStrategy` retry, `maxRetries 2`), where mrp retries only failures matching its `retry_on` patterns (signals, ENOMEM, scheduler errors) and never an ordinary stage exception. A stage that fails and then succeeds on retry completes under mro2nf and fails under mrp (mrp's `jobmanagers/retry.json`; `internal/emit/emit.go` `errorStrategy`). The outcome comparison counts a retried stage task's failures once on the failure fixtures whose error-failing stage Nextflow retries, and fails on any of them where no task is retried. Scoped to `bad_out_type`, `missing_struct_key`, `split_bad_mem`, `split_bad_special`, `split_bad_threads`, `stage_raise`. | `TestMrpDiffFailureParity/stage_raise`, `TestOutcomeRulesNeedTheirEntries` |
| `(local)` | ⚠️ mechanism, permanent | **`local-modifier-ignored`**: mro2nf ignores `local` and schedules the stage on the configured executor like any other task; in a cluster jobmode mrp runs it through its local job manager on the head node, sized against the head node's capacity. Scheduling only: the stage runs the same code on the same `_args`. Under `--jobmode=local`, which the differential uses, the local job manager is mrp's only one (`martian/core/node.go`, `stage.go`), so `kitchen_sink`'s local SUM_SQUARES matches mrp; cluster jobmodes are not compared with mrp. | `TestMrpDiff/kitchen_sink` |
| `outs/` move+symlink layout | ⚠️ mechanism, permanent | **`outs-files-copied`**: mrp moves each output file into `outs/` and leaves a symlink to it in the stage's files dir; mro2nf copies each output into `results/` and leaves the task's work dir as it was. Both publish regular files with the same paths and bytes: the tree comparison, which tells a symlink from a file, proves it on every `TestMrpDiff` fixture. Where mrp publishes a symlink instead, `published-symlinks-copied` applies. | `TestMrpDiff/file_min`, `TestMrpDiff/dir_out`, `TestMrpDiff/map_file` |
| a mapped or disabled `(preflight)` | ⚠️ mechanism, #314 | **`preflight-dag-order`**: A mapped or disabled preflight runs in DAG order, where mrp makes every other call in its pipeline wait for it, so a failing preflight stops that pipeline before they start. A preflight has no outputs (mrp rejects one with `PreflightOutputError`), so the order changes no output: `preflight_ungated`'s outputs and stage invocations match mrp's. On a failing preflight, mrp has started no other call of its pipeline, while mro2nf may have. mro2nf warns at transpile time. | `TestMrpDiff/preflight_ungated`, `TestWarningsPreflightUngateable` |
| `-resume` ≈ restart-without-rerun | ⚠️ mechanism, permanent | **`resume-content-cache`**: mro2nf resumes through Nextflow's `-resume`, which reuses each task whose inputs hash the same, where mrp restarts a pipestance from the stages it recorded as complete. A cache hit replays the task's recorded outputs: an unchanged rerun re-executes no task and leaves the published tree and `pipeline_outs.json` unchanged, and a fresh run matches mrp. A rerun after a failure is not compared with mrp's restart. | `TestResumeCachesEverything`, `TestFreshRunsAreIdentical`, `TestMrpDiff` |
| stage / pipeline `retain` | ⚠️ mechanism, permanent | **`retain-no-vdr`**: `retain` has nothing to do under mro2nf, which deletes no file, so every file is kept. `retain` only exempts a file from VDR. `vdr_volatile` retains a volatile stage's file and publishes what mro2nf does under mrp's rolling and strict VDR. | `TestMrpDiffVDR/rolling`, `TestMrpDiffVDR/strict` |
| `(volatile)` / `volatile = strict` | ⚠️ mechanism, permanent | **`volatile-no-vdr`**: mro2nf ignores `volatile` and deletes no file mid-run, so `work/` keeps every intermediate file mrp's volatile data removal (VDR) deletes once its consumers finish (higher peak disk). VDR deletes only files no later stage reads and never a published output: `vdr_volatile` publishes what mro2nf does under mrp's rolling and strict VDR, while mrp deletes the volatile file its consumer has read. | `TestMrpDiffVDR/rolling`, `TestMrpDiffVDR/strict`, `TestMrpDiff/kitchen_sink` |
<!-- END GENERATED DIVERGENCES -->

## Notes

- Every e2e fixture's expected output is the real `mrp` result, captured under
  `testdata/<name>/expected/`.
- Every Martian map-call combination is now supported, including the formerly
  guarded ones: a `disabled` map call, a map over a pipeline with an internal
  disabled call, and a nested map (a map over a pipeline that itself maps, via
  2-D composite-key fork threading). The transpiler no longer emits an
  `UnsupportedError` for any map-call shape. (The `lower` stage still guards a
  genuinely unknown expression kind as defensive code.)
- Former `mrp`-runtime-only items, now handled at the shim level:
  - **content-based retry (ASSERT vs retryable)** — ✅ done. The shim exits 42 for
    an `ASSERT:` failure and 1 otherwise; the config's dynamic `errorStrategy`
    terminates the former and retries the latter, where `mrp` retries only
    transient failures (`retried-stage-errors`, #299).
  - **`--monitor` memory enforcement** — ✅ done (opt-in `mro2nf -monitor`). The
    shim polls the stage process group's RSS and kills it over `mem_gb` (mrp's
    primary monitor kill, same "exceeded its memory quota" message), and
    additionally caps the adapter's address space via `prlimit --as` at
    `vmem_gb`. Caveat: RLIMIT_AS bounds virtual address space, so a
    high-virtual/low-resident stage may hit the vmem cap before the RSS kill;
    enable only when you want those ceilings.
  - **VDR rolling/strict mid-run deletion** — *not* shim-level (a single phase has
    no downstream-DAG view). For a volatile output with one consumer it is cleanly
    emittable (inject the delete into that consumer's shim, removing the symlink
    target); fan-out needs a common-descendant barrier or falls back to post-run
    cleanup, since "last consumer" is a runtime fact. Future work; today the whole
    `work/` tree is retained (higher peak disk, identical outputs).
  - **live UI / HTTP API** — no faithful Nextflow analog. (OOM handling is
    covered: memory directives grow with `task.attempt`, so a memory-killed
    stage retries with more — see the resources table above.)
- Scheduler-*allocation* fidelity is now broad: per-chunk `cpus`/`memory`, the
  split-returned `join` override, and `special` → `clusterOptions` all reach the
  scheduler, so a transpiled run provisions like mrp and runs in a comparable
  duration, while every phase reports mrp's resolved request in `_jobinfo`. A
  purely split-returned `__special` is now routed even with no static key (the
  main/join phases read the per-task `__special`), and a declared `using(vmem_gb)`
  is passed to the shim via `-vmemgb` on every phase, so `--monitor` caps virtual
  memory (and `_jobinfo` reports it) at the declared value (a per-chunk
  `__vmem_gb` still wins for main, a split-returned join `__vmem_gb` for join).
- The published `outs/` tree reproduces mrp's nested layout and
  `GetOutFilename` naming (verified by the Go `TestMrpDiff` suite against real
  mrp). Every remaining ⚠️ item is a row of
  [Divergences from mrp](#divergences-from-mrp).
- **Embedded absolute paths (shared-filesystem assumption) — unsupported on
  object stores, flagged loudly.** The data plane stages *declared* file leaves;
  a path a stage bakes into another file's *content* (e.g. a JSON manifest that
  names other files by absolute path) is invisible to the type walk and cannot
  be relocated. Under `mrp` (shared filesystem) it resolves; on AWS Batch + S3 /
  HealthOmics the downstream isolated container never sees the producer's
  scratch, so the referenced path is absent. This is a stage authoring pattern
  the transpiler cannot faithfully reproduce, not a transpiler bug. The shim
  scans each small text stage output for its task's own scratch-dir prefix and
  emits a loud `mre: WARNING` (to the task log → CloudWatch / the Omics engine
  log) naming the output and the offending path, turning a cryptic downstream
  `FileNotFoundError` into a clear, stage-local diagnostic at production time.
  (Best-effort: small text outputs only; binary/large outputs are skipped.)
