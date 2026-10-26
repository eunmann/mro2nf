# Martian feature coverage

Every Martian feature, whether mro2nf supports it, and the tests that prove
it. The features come from a page-by-page audit of the seven martian-lang.org
docs (writing-pipelines, language-details, writing-stages, advanced-features,
storage-management, running-pipelines, inspecting-pipelines), cross-checked
against the `martian/syntax` grammar.

Status:

- ✅ supported and tested;
- ⚠️ supported with a divergence from `mrp`; every one is a row of
  [Divergences](divergences.md), generated from the allowlist the tests apply;
- ❌ unsupported, refused with a clear error, and tracked;
- 🚫 part of `mrp`'s runtime, out of scope for a transpiler.

| Test type | Meaning |
|---|---|
| **e2e** | transpiled, run under Nextflow, and diffed against real `mrp`: the committed golden, and the live `mrp` differential `TestMrpDiff`, which also compares every stage's `_args`, `_outs` and `_jobinfo` |
| **unit** | a Go unit test |
| **corpus** | `TestCorpusParamsResolve`, which compiles each `testdata/corpus` file and resolves every parameter's type |

The generated project runs on Nextflow 26.04.0 or later, the floor the nightly
suites run beside the latest stable release, under its default strict (v2)
syntax parser, and lints warning-free (`make lint-nf`).

> [!NOTE]
> **Nothing unsupported fails silently.** The call graph is Martian's own, so a
> pipeline Martian's compiler or call-graph builder refuses fails the transpile
> with Martian's message and writes no project, and a run invocation it refuses
> fails the run's PLAN task before any stage starts. A shape whose data the
> tasks cannot exchange (a fork root several stages feed, a disable binding
> forked on a map call another stage feeds) fails the transpile too.

## Language — writing-pipelines / language-details

| Feature | Status | Test |
|---|---|---|
| stage / pipeline / top-level call | ✅ | e2e `split_test`, all |
| top-level call targeting a bare **stage** | ✅ | e2e `stage_entry` |
| top-level **`map call`** | ✅ forks the entry pipeline per element of its split input, a launch value in Martian's `{"split": v}` form forking by `v`; its outs are the pipeline's outs per fork, an array or a map, each fork's files published under `<fork key>/`, as `mrp` publishes them | e2e `TestMrpDiff/map_call_entry`, `TestMrpDiffSplitEntryOverride`; unit `TestWriteOutsOfATopLevelMapCall`, `TestPlanForksByTheSplitValue` |
| entry inputs as run parameters (override at launch) | ✅ each entry input → `params.<name>`, whose default in `main.nf`'s `params` block is the call's value (unset keeps the transpiled value; a `-params-file` null sets the input to null, as a null literal in `mrp`'s invocation does; a string input takes any text); PLAN's `mre plan` overlays a `-params-file` or `--<name>` value on the invocation and recompiles it, so a launch value changes forks, gates and pruned calls as a literal would; `main.nf`'s `params` block declares every param, so an undeclared one fails the launch, and types the `string` inputs; CLI strings are parsed into the declared scalar int/float/bool, a value of another type fails the run at PLAN naming the input and its type, a split input of the top-level `map call` takes Martian's `{"split": v}` form and forks by `v`, every override number reaches `_args` as `mrp` encodes the literal it spells (`2.0` → `2`, a whole number to a `float` input exact), and a value holding a float beyond float64 fails the run at PLAN naming the input, as `mrp` refuses the literal; so does an int outside int64, and a YAML or command-line `-0.0` reaches it as `-0`, but a JSON `-params-file` wraps that int and reads `-0.0` as `0.0` before PLAN (`params-json-int-wrap`); `nextflow_schema.json` (nf-schema format) gives every param's JSON Schema type and default for launch forms, a split input's in the `{"split": v}` form, and validates nothing at launch; an input named with a Groovy or Nextflow keyword, which the `params` block cannot declare, fails the transpile (`keyword-input`) | unit `TestTypeOverrides`, `TestEmitDeclaresEveryLaunchParam`, `TestEmitPassesEveryEntryValue`, `TestGroovyLiterals`, `TestPlanSetsNullValues`, `TestEmitRefusesKeywordInput`, `TestNextflowKeyword`, `TestPlanNestedEntryForks`, `TestPlanForksByTheSplitValue`, `TestPlanRefusesOutOfRangeValues`, `TestPlanFloatValuesAsMrp`, `TestPlanNestedNumbersAsMrp`, `TestSchema`, `TestSplitSchema`, `TestSplitInputSchema`, `TestEmitParamsSchema`, `TestParamsSchemaValidatesTypes`, `TestEmitEveryFixture` (each schema against nf-schema's meta-schema); e2e `TestCLITypedOverrides`, `TestUndeclaredParamFailsAtLaunch`, `TestOutOfRangeLaunchValues`, `TestMrpDiffSplitEntryOverride`, `TestMrpDiffInt64Override`, `TestMrpDiffFloatOverride`, `TestMrpDiffNullOverride`, `TestMrpDiffStringOverride`, the golden `*_override` cases |
| file-typed entry inputs | ✅ an absolute path is read in place by every task on the `standard` and grid profiles, whose tasks run on the host's filesystem: the stage receives the path `mrp` passes, a path one stage records for another to open holds in every task, a stage returning its input's path posts the path with no copy, and a path that is or holds the run's work dir is refused at PLAN. A URI, and on the container profiles an absolute path too, is staged by Nextflow into only the tasks that read it, each fork of a map call over entry files its own element; the stage gets a path linked below its task's args dir, `args/f/L<nnnn>/<name>`, with the value's file name and content (divergence `staged-input-paths`), and a file beside it is not staged (divergence `input-file-siblings`). A relative path names nothing from the stage's job dir under either runtime. `s3://` on every profile, a head-node path where the profile's tasks see the head node's files, other schemes only with `--mro2nf_allow_remote_inputs true`; a staged name Nextflow's stage-in script would expand (`*?[]{}$`) is refused at PLAN ([Inputs](../architecture/inputs.md#names-nextflow-cannot-stage)). On `awsbatch` an `s3://` value of a bucket the compute environment mounts is not staged: every task reads it in place at `/mnt/s3/<bucket>/<key>` through a read-only mount, the path the stage receives; a value of any other bucket is staged. `-resume` keys a value read in place on its path, as `mrp`'s restart keys its invocation | e2e `entry_file`/`entry_filearr`/`entry_struct_file`/`entry_mapfile`/`entry_dir`/`entry_file_empty`/`mounted_paths` (`_override`: absolute paths; the defaults: `TestMrpDiffFailureParity`), `entry_file_sibling`, `TestMrpDiffHostEntryPaths`, `TestDockerEntryOverrides`, `TestEntryFileAdversarialName`, `TestEntryFileGlobCharName`, `TestEntryFileRemoteURL`, `TestDockerMountedS3Inputs`; unit `TestPlanStagesEachForksOwnEntryFiles`, `TestVetEntryLeaf`, `TestVetHostLeaf`, `TestArgsReadsStagedEntryFiles`, `TestPlanMountsS3EntryFiles`, `TestPlanStagesS3EntryFilesOfUnmountedBuckets`, `TestRootsAreThePathsReadInPlace`, `TestStageKeepsPathsReadInPlace`, `TestMountedLeafStaysItsPath`, `TestRootsExceptTheTaskDir`, `TestEmitHostInputsInPlaceOffContainers` |
| pipeline with **no calls** (return-only) | ✅ | e2e `returnonly` |
| `@include` (resolved via `-mropath`, default `$MROPATH`, else the `.mro`'s directory, as `mrp`; never the working directory) | ✅ | e2e `include_test`; unit `cmd/mro2nf` `TestRunDefaultSearchPath`, `TestSearchPathReachesOptions`, `internal/cg` `TestSourceIncludesIgnoreWorkingDir` |
| `@include` diamond / cycle / missing | ✅ (parser-enforced) | via `martian/syntax` |
| `filetype` | ✅ | e2e `file_min`, `kitchen_sink` |
| `struct` types | ✅ | e2e `struct_min`, `struct_proj`, corpus |
| named / unnamed (`default`) out params | ✅ | e2e `default_out` |
| out filename (`out txt r "" "f.txt"`) | ✅ | e2e `file_min` |
| scalar types int/float/string/bool/path/file/map | ✅ | e2e (various) |
| typed arrays `T[]`, multi-dim `T[][]` | ✅ | e2e `multidim`, `split_test` |
| typed maps `map<T>` (output) | ✅ | e2e `typedmap_out` |
| int→float coercion; float→int rejected | ✅ | `martian/syntax` |
| whole-number float literal → int param (`5.0` → `5`) | ✅ each bound value goes through its type's `FilterJson`, as mrp resolves it; a split-returned chunk arg reaches main and join as the split wrote it | e2e `float_to_int`, `chunk_float_int` |
| literals: int/float/sci, bool, null, arrays, maps, struct literals, escapes | ✅ | `martian/syntax`; e2e entry args |
| bindings: `self.x`, `self.x.y`, `STAGE`, `STAGE.out`, `STAGE.out.field`, `STAGE.default` | ✅ Martian's `ResolvedBinding.BindingPath` and `LazyArgumentMap.Path` | e2e `struct_proj`, `default_out` |
| field projection through arrays, maps and arrays of maps (`CALL.s.field`, `map<S>.field`, `array<map<S>>.field`) | ✅ Martian's own projection | e2e `struct_proj`, `map_struct_proj`, `arr_map_proj`; unit `TestMergeProjection` |
| literal edge cases: negative numbers, `>2^53` int64 precision, unicode/escape strings, float lexemes in the outs (`1e+21`, `-0.0`) | ✅; an int literal outside int64, a float beyond float64, or a stage's `threads`, `mem_gb` or `vmem_gb` beyond float32, on which `mrp` panics, fails the transpile naming its file, line and column | e2e `literals_edge`, `literals_big`, `float_lexemes`, `TestWorkflowIndexRespellsNumbers`; unit `TestReadSourceRefusesOutOfRangeLiterals` |
| wildcard binding `* = self` / `* = REF` | ✅ | e2e `wildcard` (compiler expands) |
| **refs nested in array/map literals** (`[A.x, B.x]` fan-in) | ✅ | e2e `fanin` |
| struct duck typing (extra fields) | ✅ every bound value is narrowed to the receiving param's declared struct by its type's `FilterJson` (extra fields and their file leaves dropped, a missing field null), in stage `_args` and pipeline outputs | e2e `struct_narrow` |
| struct and map key order | ✅ #366 a value reaches a stage's `_args` and `pipeline_outs.json` in mrp's key order | e2e `struct_key_order`, `nested_struct`, `kitchen_sink`; unit `TestKeyOrder` |
| null inputs / null propagation | ✅ | e2e `null_in`, `modifiers_min` |

## Calls & modifiers

| Feature | Status | Test |
|---|---|---|
| aliased calls `call X as Y` (repeated) | ✅ | e2e `alias_min`, `kitchen_sink` |
| nested pipeline calls (pipeline→pipeline) | ✅ | e2e `modifiers_min`, `kitchen_sink` |
| non-linear / diamond DAGs | ✅ | e2e `diamond_min` |
| `using (disabled = ref)` (self or call ref) | ✅ decided per instance as mrp's `Fork.disabled` decides it: a gate that is null at run time leaves the call enabled; a gate the invocation fixes is PLAN's, one a stage's outs set is that stage's posted gates, both evaluated on the driver, so a disabled instance runs no task. A gate null before any stage runs fails with mrp's `disabled cannot be bound to a null value` binding error | e2e `modifiers_min`, `null_gate`, `chain_gate`, `split_gate`, `split_gate_entry`; unit `TestRuntimeDisable`, `TestStaticDisable`, `TestNullGateEnables`, `TestPostGates` |
| whole-call ref (`s = CALL`) to a **disabled** stage or pipeline call | ✅ null, as mrp | e2e `disabled_wholeref` |
| `(preflight)` | ✅ every other call of its pipeline, recursively into sub-pipelines and their preflights, waits for every instance of each preflight, a disabled one counting as done, as mrp makes a preflight a prenode of each; a failing preflight ends the run before they start | e2e `TestPreflightFailsBeforeItsPipeline`, `TestMrpDiff/preflight_order`, `TestMrpDiff/preflight_ungated`, `TestMrpDiffFailureParity/preflight_fail`; unit `TestTopologyPreflights`, `TestEmitWaitsForPreflights` |

## Map calls / forks — writing-stages / advanced-features

| Feature | Status | Test |
|---|---|---|
| `map call ... split` over an **array** → array result | ✅ | e2e `fork_min` |
| `map call ... split` over a **map** → keyed result | ✅ fork elements in mrp's order, map keys in byte order; a key of any characters crosses only inside mre's records | e2e `map_fork`, `map_key_sort`; unit `TestForkElementKeys`, `TestForkRecordRoundTrip` |
| zipped multi-`split` | ✅ forks over its master source; the other sources read null past a shorter one's end | e2e `multisplit`, `zip_runtime`, `zip_literal`; unit `TestZip*` |
| whole-call ref (`x = CALL`) to a **map call** → one struct per fork | ✅ | e2e `map_wholeref`; unit `TestWholeCallRefs` |
| empty or null split collection → typed empty result (`[]`, `{}`) | ✅ merged as mrp merges it, statically or at run time (Martian 50ad6c0; null before it) | e2e `empty_fork_*`, `runtime_empty_forks`; unit `TestEmptyForksPrune` |
| map call over a **split stage** | ✅ | e2e `map_split` |
| map call over a **sub-pipeline** | ✅ | e2e `map_pipe`, `map_pipe_split`, `fork_indep_call` |
| map call emitting a **file** (array/keyed-map of files) | ✅ | e2e `map_file`, `map_file_keyed`, `map_file_split` |
| **complex combo**: map over a sub-pipeline whose body is a split stage emitting a file (Cell Ranger per-sample pattern) | ✅ | e2e `map_split_file` |
| **complex combo**: py + exec + comp adapters chained in one pipeline | ✅ | e2e `mixed_adapters` |
| **complex combo**: struct output whose field is a file array | ✅ | e2e `struct_file_array` |
| empty-split fork inside a map call (0 chunks → 0/null, no fork dropped) | ✅ | e2e `map_split` (includes an empty fork) |
| `disabled` on a map call | ✅ decided per fork | e2e `disabled_map`, `disabled_map_ref`, `disabled_map_nullforks` |
| map call over a pipeline with an internal **disabled** call | ✅ | e2e `map_pipe_disabled` |
| **nested** map call (a map over a pipeline that itself maps) | ✅ a root whose forks vary with an outer map call gets its forks under each fork of that call. A merge Martian expands per fork of an outer map call of known length gathers, as `mrp` does, the inner forks of the element's own outer fork, an empty one as `[]`, whatever the other outer forks hold (`map_pipe_nested`, `merge_across_short`). An inner split of the outer split's elements, all empty and known at run start (`xss = [[]]`), fails the run, as under `mrp` (divergence `nested-empty-forks`); an inner split of a bound empty literal or of an all-empty stage output runs (`fork_indep_call`) | e2e `fork_indep_call`, `bench/mapnested`, `TestMrpDiff/merge_across_outer`, `TestMrpDiff/merge_across_empty`, `TestMrpDiff/merge_across_short`, `TestMrpDiff/map_pipe_nested`; unit `TestNestedEntryForkNames`, `TestNestedForkUnderRuntimeOuter`, `TestPlanNestedEntryForks`, `TestMergeAcrossOuterForks`, `TestMergeAcrossOuterForksOfEveryShape` |
| **disabled** nested map call | ✅ | e2e `map_pipe_disabled_nested` |
| map call over another map call's outs (`map call B(x = split A.y)`, A over a stage's outs) | ✅ B's forks come from the source A splits over, as `mrp` sizes them from Martian's compiled split source: the stage that fixes A's forks posts B's, and B's fork i reads A's fork i | e2e `map_chain_runtime` |
| map call over a **runtime-gated** output (`split X.o` where a stage output disables X) | ✅ the forks come from the gated value where the gate holds it enabled, and there are none where it disables it, as Martian 50ad6c0 expands them: the gated call's own forks for a map call of known length (each reading null with the gate on), else the gate's stage or the gated stage | e2e `rt_gate_split`, `rt_gate_split_sub`, `rt_gate_split_pass`, `rt_gate_split_on`, `empty_fork_gated_sub`; unit `TestGatedSplitTranspilesAsMrp` |

## Stages / adapters / resources

| Feature | Status | Test |
|---|---|---|
| split / main / join | ✅ one task per chunk, as mrp runs one job per chunk | e2e `split_test`, `kitchen_sink`; unit `TestStageTasks` |
| ordered chunk-outs aggregation in join | ✅ by chunk name, never completion order | e2e `jitter_wide`; unit `TestJoinWritesChunkFiles` |
| `py` adapter | ✅ through `mrjob`, as mrp runs it | e2e (all py); unit `TestArgvIsMrps` |
| `exec` adapter | ✅ | e2e `exec_min`, `src_args` |
| `comp` adapter (through `mrjob`) | ✅ | e2e `comp_split`, `mixed_adapters` |
| per-chunk resources `__mem_gb/__threads/__vmem_gb/__special` | ✅ in `_args`/`_jobinfo`, through Martian's own types; a value of the wrong JSON type fails the split, as under mrp (#275) | e2e `TestMrpDiffFailureParity` (`split_bad_special`, `split_bad_mem`, `split_bad_threads`), `join_resources`, `res_pool`; unit `TestLocalGrant`, `TestClusterGrant` |
| per-chunk and split-returned join resources → Nextflow scheduler directives | ✅ the driver sizes each chunk and the join from the `resources.tsv` the split wrote, the cpus, memory and special each job's `_jobinfo` records | e2e `split_test`, `join_resources`; unit `TestEmitSizesChunksAndJoin` |
| resources a stage sees (`__*`, `_jobinfo`) | ✅ #243 #370 #545 each job's `_jobinfo` and `_args` carry its request as mrp grants it in the jobmode its profile runs as: on `standard` and `docker` as mrp's local job manager grants it from the pool `process.resourceLimits` sets, fractions kept, an adaptive request the whole pool and every request but vmem capped at it; on the grid, `awsbatch` and `healthomics` profiles as mrp's cluster job managers grant it, an adaptive request at its magnitude, threads whole, nothing capped. The task requests the cpus, rounded up, and memory its job is granted. An adaptive memory request: divergence `adaptive-memory-pool` | e2e `frac_mem`, `frac_threads`, `res_pool` (mrp ABI diff), `TestGeneratedHealthOmicsPackage`; unit `TestMainWritesMrpMetadata`, `TestLocalGrant`, `TestClusterGrant`, `TestPlanGrantsFromThePool`, `TestLocalProfilesGrantFromThePool` |
| `using(special)` → scheduler | ✅ `clusterOptions` from `params.job_resources[special]` (the `MRO_JOBRESOURCES` analog); `special = "gpu"`/`"gpu:N"` also requests GPUs, for every job whose special it is, a split's chunks and join by the `__special` it returned ([GPU stages](../user-guide/resources-and-retries.md#gpu-stages)) | e2e `special_resource`, `gpu_stage` (mrp ABI diff), `TestGPURequestFollowsSpecial`; unit `TestEmitGPURequest` |
| `martian` module API (make_path, log_info/log_warn, update_progress, exit) | ✅ (real adapter drives it) | e2e `kitchen_sink`, `file_chain`, `api_smoke` |
| `martian.alarm` | ✅ each job writes every `_alarm` line to its task's stderr, where mrp prints `Alerts for <stage>:` as the stage completes. No run-level `Alerts:` summary on the Nextflow console | unit `TestAlarmsReachStderr` |
| `martian.get_memory_allocation` / `get_threads_allocation` (read `_jobinfo`) | ✅ | e2e `api_smoke` |
| directory-typed (`out path`) output published as a tree | ✅ its empty dirs reach its readers and the published tree through an object store (#624) | e2e `dir_out`, `dir_hidden`; unit `TestReaderRestoresEmptyDirs`, `TestStageRestoresWhatAnObjectStoreDrops`, `TestWriteOutsPublishesEmptyDirs` |
| a link a stage wrote that reaches nothing, inside a directory output | ✅ its readers see it, with its target (#624); published, it fails the run: divergence `dir-dangling-link` | e2e `dir_dangling_link` (mrp diff, with a copy-staged leg), `TestMrpDiffFailureParity` (`dir_dangling_link_out`); unit `TestReaderRestoresDanglingLinks`, `TestWriteOutsRefusesAStageDanglingLink` |
| stage output validation (mrp `verifyOutput`) | ✅ the post step of `mre stage` checks a stage's final outs with Martian's `ValidateOutputs` and fails the task before any consumer binds them | e2e `TestInvalidOutputFailsLikeMrp`, `TestMrpDiffFailureParity` (`missing_struct_key`, `bad_out_type`); unit `TestPostRejectsInvalidOuts` |
| ASSERT vs retryable-error classification | ✅ `mre stage` exits 42 when its job writes an `_assert`, 75 for a failure a `retry_on` pattern of `mrp`'s `jobmanagers/retry.json` matches, and 1 for any other; the config retries only exit 75 and an executor kill or timeout (104, 130–145) | unit `TestOutcomes`, `TestExitCode`; e2e `TestAssertTerminatesWithoutRetry`, `TestMrpDiffFailureParity` (`assert_min`, `assert_split`, `assert_join`, `stage_raise`) |
| OOM escalation | ✅ none, as in `mrp` without `--auto-adjust-memory`, which has no `mro2nf` counterpart: a stage `--mro2nf_monitor true` kills over its request fails the run, and a retried task, such as one the executor killed by a signal, requests the memory and cpus of its first attempt, Martian's resolution of its job's request (#506); the stage sees that request in `_jobinfo` and `__mem_gb`. A `-c` config may set `memory = { … * task.attempt }` to escalate ([Retries](../user-guide/resources-and-retries.md#retries)) | unit `TestEmitRetriesWhatMrpRetries`, `TestOutcomes`; e2e `TestRetryRequestsTheSameResources`, `TestMonitorMemoryKillFailsLikeMrp` (`flaky_retry`) |
| thread-limit env vars (mrp's `thread_envs`) | ✅ every job runs with mrp's thread env vars set to its granted threads rounded up, over any inherited value (`internal/job` `threadEnvs`) | e2e `res_pool` (mrp diff); unit `TestThreadEnvs` |
| `--monitor` memory enforcement | ✅ with `nextflow run main.nf --mro2nf_monitor true`: `_jobinfo`'s `monitor_flag`, which `mrjob` enforces as under mrp | unit `TestMonitorFlag` |

## Outputs / storage — storage-management

| Feature | Status | Test |
|---|---|---|
| file outputs published to results (mrp `outs/` layout, `GetOutFilename` naming) | ✅ OUTS lays the tree out with `mrp`'s post-processing from the outs' skeletons, none of their bytes; the workflow outputs (`output {}`) copy each file from the task that wrote it, and an entry or host file from OUTS's copy, below Nextflow's `outputDir` (`results`, `-output-dir`, a `-c` config's `outputDir`) | e2e `file_min`, `TestMrpDiff` (tree diff vs real mrp), `TestMrpDiffPublishesHostEntryFiles`, `TestDockerIsolation`, `TestOutputDirFromConfig`, `TestGeneratedAWSBatchProfile`, `TestBench` (publish gate); unit `TestEmitPublishesThroughWorkflowOutputs`, `TestWriteOutsPublishesAsMrp` |
| inter-stage file passing | ✅ each task stages the bundles it reads, and the files they name by reference where their producer wrote them, as `path` inputs; markers locate the leaves ([Bundles and file flow](../architecture/bundles.md)) | e2e `file_chain`, `TestCloudSim*`; unit `TestBundleRoundTrip` |
| a path written into text (JSON or CSV contents, an untyped `map`, a `string`) that a later job opens | 🚫 relies on `mrp`'s shared filesystem; no task stages it, so it opens only where the later task sees that path. Per profile in [Paths written into text](../user-guide/files-and-paths.md#paths-written-into-text) | e2e `TestDockerMountedS3Inputs` (a file input's path in JSON, under copy-staging as on `awsbatch`), `TestMrpDiffHostEntryPaths` (a head-node input's path in JSON, on `standard`) |
| file-array / struct-of-file / map-of-file outputs publishing | ✅ | e2e `map_file`, `map_file_keyed`, `struct_of_file` |
| a file reached from several outputs | ✅ #431 named by the first path mrp published it at | e2e `shared_out_file` |
| a stage output naming a file in the pipeline's code (its include path or a stage's code directory) | ✅ #625 the path stays the path, as `mrp` keeps it, on every profile, a container profile's confinement included, since every task finds the code at the path it was transpiled from; a later stage reads the file there, and OUTS publishes a copy | e2e `code_path_out` (`TestGolden`, `TestMrpDiff`, `TestDockerIsolation`); unit `TestStageKeepsPathsIntoTheCode`, `TestCodeRootsHoldNoTaskOfTheRun`, `TestCodeRootsKeepPathsNotLinks` |
| VDR modes (disable/post/rolling/strict) timing | 🚫 no mid-run deletion | e2e `TestMrpDiffVDR/rolling`, `TestMrpDiffVDR/strict` (published outputs match mrp's under its VDR) |

## Running / inspecting — running-pipelines / inspecting-pipelines

| Feature | Status | Test |
|---|---|---|
| executor / jobmode (local + slurm/sge/lsf/pbs) | ✅ config profiles; the local ones run as mrp's local jobmode, granting each job from the host's cpus and 90% of its memory and clamping its task to them, the others as a cluster jobmode | e2e `TestLocalClampRunsOversizedRequest`; unit `TestLocalProfilesGrantFromThePool` |
| a failure is retried only when `mrp` retries it | ✅ `mre` exits 75 for a job failure a line of whose `_errors` matches a `retry_on` pattern of `mrp`'s `jobmanagers/retry.json` (a signal, ENOMEM, a scheduler failure); the config retries only exit 75 and an executor kill or timeout (104, 130–145, the exits nf-core pipelines retry), `maxRetries 2` as `default_retries`. An ASSERT (42), a stage exception or a failed `mre` step, which would fail again, ends the run on its first attempt | unit `TestOutcomes`, `TestExitCode`, `TestEmitRetriesWhatMrpRetries`, `TestRetryPatternsMatchModule`; e2e `TestAssertTerminatesWithoutRetry`, `TestMrpDiffFailureParity`, `TestMonitorMemoryKillFailsLikeMrp` |
| isolated containers, AWS Batch, HealthOmics | ✅ profiles of one project (`docker`, `awsbatch`, `healthomics`): every task stages what it reads, a head-node entry input included, but for the `s3://` entry inputs `awsbatch` reads through the read-only S3 mount its instances serve where they mount the input's bucket (`deploy/awsbatch-cdk`), the run's call reaches it as a value, or, above 4 KiB, as a file the driver writes to the work dir, every task runs in the one image `--mro2nf_image` names, which the user builds FROM the release runtime image with the pipeline's code, a launch without it fails before PLAN, and the HealthOmics package pins it by digest. No CI job runs on AWS: `.github/workflows/cdk-validation.yml` only synthesizes the example infrastructure in `deploy/awsbatch-cdk` | e2e `TestDockerIsolation`, `TestDockerEntryOverrides`, `TestGeneratedAWSBatchProfile`, `TestDockerMountedS3Inputs`, `TestCDKMountsWhereTheProfileReads`, `TestGeneratedProfilesRequireImage`, `TestGeneratedHealthOmicsPackage`; unit `TestEmitTargetsAsProfiles`, `TestEmitMountsS3InputsOnAWSBatch`, `TestEmitHealthOmicsTemplate` |
| `mrp --overrides` (per-stage resource retune at launch) | ✅ `--mro2nf_overrides <file>`: PLAN reads it with Martian's `ReadOverrides`, so an unknown key fails the run at its start, and each node's split, chunk and join resources resolve through Martian's `ApplyResourceOverrides`, nearest key first, into the task's `cpus`/`memory` and the job's `_jobinfo` and `_args`; each task keys `-resume` on its own node's overrides alone. `force_volatile` and `*.profile` change no output and are read without effect. See [`mrp --overrides`](../user-guide/resources-and-retries.md#mrp---overrides) | e2e `overrides_resources` (mrp ABI diff), `TestMrpDiffFailureParity` (`overrides_unknown`), `TestResumeKeysOnANodesOverrides`; unit `TestResourcesUnderOverrides`, `TestOverrideGivesTheNodeItsResources`, `TestPlanResolvesOverrides`, `TestStageUnderOverrides` |
| `--maxjobs` / `--jobinterval` / `--localcores` / `--localmem` (throttling) | ✅ in a `-c` config: `executor.queueSize`; `executor.submitRateLimit` at whole-second intervals (`'<n>/1s'`), as Nextflow rejects a shorter one; `process.resourceLimits` cpus and memory, the pool each job is granted from and its task clamped to, as `mrp`'s local job manager grants from `--localcores`/`--localmem`, with `executor.cpus`/`memory` to bound what runs at once. Mapping in [`mrp` equivalents](../user-guide/mrp-equivalents.md) | `TestLocalClampRunsOversizedRequest`, `TestMrpDiff/res_pool` |
| `--profile` (stage profiling) | 🚫 the task trace the project enables (`realtime`, `%cpu`, `peak_rss`, `peak_vmem`, `rchar`, `wchar`) and `-with-report` | — |
| `--inspect` | 🚫 no analog; `nextflow log` lists past runs and their tasks | — |
| `--onfinish` (hook) | 🚫 a `workflow.onComplete` handler in a `-c` config, which can pass the hook `mrp`'s outcome (`complete`/`failed`) and pipestance id; there is no pipestance path or error file to pass ([`--onfinish`](../user-guide/mrp-equivalents.md#--onfinish)) | — |
| web UI / HTTP API / mrstat | 🚫 use `nextflow log`/`-with-report` instead | — |
| `mro check` / `mro graph` | 🚫 not run by mro2nf or its tests; a transpile fails on a pipeline Martian's compiler or call-graph builder refuses. `mro graph` ≈ `nextflow run -with-dag` | — |

## Notes

- Every e2e fixture's expected output is the real `mrp` result, captured under
  `testdata/<name>/expected/`. A fixture `mrp` fails on is a failure-parity case
  (`TestMrpDiffFailureParity`) and has no golden.
- The call graph, its forks and every stage's `_args` are Martian's own: `mre`
  compiles the run's invocation with Martian's compiler and resolves each
  instance with Martian's binding, projection and type functions, and with
  Martian core's per-instance resolution ported to read what a task staged
  ([The `mre` shim](../architecture/mre.md#the-args-step)).
- Former `mrp`-runtime-only items:
  - **content-based retry (ASSERT vs retryable)** — `mre stage` exits 42 when
    its job writes an `_assert`, 75 for a failure `mrp`'s `retry_on` patterns
    name transient, and 1 otherwise; the config retries only exit 75 and an
    executor kill or timeout (104, 130–145), a failure that can pass on a
    second attempt (#299).
  - **`--monitor` memory enforcement** — `--mro2nf_monitor true` sets `_jobinfo`'s
    `monitor_flag`, which `mrjob` enforces as under `mrp`.
  - **VDR rolling/strict mid-run deletion** — not implemented: the whole `work/`
    tree is retained (higher peak disk, identical outputs).
  - **live UI / HTTP API** — no faithful Nextflow analog.
- Scheduler-*allocation* fidelity: per-chunk `cpus`/`memory`, the split-returned
  `join` override, and `special` → `clusterOptions` all reach the scheduler, so
  a transpiled run provisions like mrp, while every job reports mrp's resolved
  request in `_jobinfo`.
- The published `outs/` tree reproduces mrp's nested layout and
  `GetOutFilename` naming (verified by the Go `TestMrpDiff` suite against real
  mrp). Every remaining ⚠️ item is a row of [Divergences](divergences.md).
- **Paths written into text are out of scope.** The tasks stage *declared*
  file leaves; a path a stage writes into text (a JSON manifest, a CSV, an
  untyped `map`, a `string`) is invisible to the type walk. It opens where the
  later task sees that path: on `standard` and grid always; under `docker`
  for a stage's output or a remote input, both under the work directory that
  a task staging a file from it mounts, but not for a head-node input, a link
  to a host path only the tasks staging it mount; on
  `awsbatch` only under the S3 mount; on `healthomics` for an input or a
  stage's output on the run's shared storage
  ([Paths written into text](../user-guide/files-and-paths.md#paths-written-into-text)).
