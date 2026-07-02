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

Status: ✅ supported & tested · ⚠️ supported with a documented divergence ·
❌ unsupported, guarded with a clear error & tracked · 🚫 runtime/`mrp`-only,
out of scope for a transpiler.

**How unsupported features are signalled (never silent):**
- **❌ hard error** — the transpile fails (`UnsupportedError` / parse error,
  non-zero exit): an unknown expression (e.g. `sweep`), an unknown adapter
  language, a `comp` stage without `mrjob`, a nested typed-map projection
  (`map<map<S>>.field`), an invalid `-target`. Lowering errors by default on any
  unrecognized expression, so a construct cannot be silently dropped.
- **⚠️ warning** — the project transpiles but a no-op divergence is **logged**
  by `mro2nf`: `local`, `volatile` (no VDR), and a `preflight` bound to a call
  output (it runs in DAG order rather than gating early).
- **🚫 / layout** — documented here with no output-observable impact.

## Language — writing-pipelines / language-details

| Feature | Status | Test |
|---|---|---|
| stage / pipeline / top-level call | ✅ | e2e `split_test`, all |
| top-level call targeting a bare **stage** | ✅ | e2e `stage_entry` |
| entry inputs as run parameters (override at launch) | ✅ each entry input → `params.<name>` (defaults to the baked `.mro` value); BUILD_ENTRY_ARGS (`mre entryargs`) overlays a `-params-file` / HealthOmics run params on the defaults | unit `TestEmitEntryParams`; verified default + override |
| file-typed entry inputs (FASTQ etc.) overridable at launch | ✅ **every shape** — scalar `file`/`path`/dir, `file[]`, `map<file>`, and struct-with-file. The value's file leaves are flattened to a list (canonical type-walk order: arrays in index order, maps by sorted key, struct fields in order), `file()`'d on the head node and staged as one `path` input, so an isolated worker reads real localized files; `mre entryargs -fileflat` pops the staged paths back into the value and marks them into the bundle. An S3 URI / `s3://` prefix (directory) is staged by Nextflow / HealthOmics | e2e `entry_file`/`entry_filearr`/`entry_mapfile`/`entry_struct_file` (+ `_override`); `docker_iso` overrides with inputs **outside the image** (arrive only via staging); live Batch + HealthOmics; unit `TestEmitEntryFileParam`, `TestEmitEntryFileArray`, `TestEmitEntryStructFile`, `TestReconstructFiles`, `TestCopyTreeDerefSymlink` |
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
| whole-number float literal → int param (`5.0` → `5`) | ✅ (type-directed `CoerceScalars`) | e2e `float_to_int`, unit `TestCoerceScalars` |
| literals: int/float/sci, bool, null, arrays, maps, struct literals, escapes | ✅ | `martian/syntax`; e2e entry args |
| bindings: `self.x`, `self.x.y`, `STAGE`, `STAGE.out`, `STAGE.out.field`, `STAGE.default` | ✅ | unit `bind`; e2e `struct_proj`, `default_out` |
| **field projection through arrays** (`CALL.s.field`) | ✅ | unit `TestResolveArrayProjection`, e2e `struct_proj` |
| field projection through maps (`map<S>.field`) | ✅ (emitter computes a per-ref `MapDepth` from the program types; the binder projects the field over the map's values — covers declared `map<S>` outputs and map-fork-induced maps) | e2e `map_struct_proj`, unit `TestResolveMapProjection`, `TestMapProjectDepth` |
| field projection through an **array of typed maps** (`array<map<S>>.field`) | ✅ the binder descends the array and projects the field over each map's values, preserving the array shape (`-> array<map<field>>`); the emitter marks it with `Ref.MapInArray` | unit `TestResolveArrayOfMapProjection`, `TestMapProjectDepth`, `TestCheckSupported` |
| field projection through **nested typed maps** (`map<map<S>>.field`) | ❌ rejected at emit time (`checkSupported` → `ErrUnsupported`) — one projection level cannot faithfully lower nested maps, so it errors rather than mis-projecting | unit `TestCheckSupported`, `TestMapProjectDepth` (depth −1) |
| literal edge cases: negative numbers, `>2^53` int64 precision, unicode/escape strings | ✅ | e2e `literals_edge` |
| wildcard binding `* = self` / `* = REF` | ✅ | e2e `wildcard` (compiler expands) |
| **refs nested in array/map literals** (`[A.x, B.x]` fan-in) | ✅ #14 | unit `bind`/`fork`, e2e `fanin` |
| struct duck typing (extra fields) | ⚠️ via JSON pass-through | corpus parse |
| null inputs / null propagation | ✅ | e2e `null_in`, `modifiers_min` |

## Calls & modifiers

| Feature | Status | Test |
|---|---|---|
| aliased calls `call X as Y` (repeated) | ✅ | e2e `alias_min`, `kitchen_sink` |
| nested pipeline calls (pipeline→pipeline) | ✅ | e2e `modifiers_min`, `kitchen_sink` |
| non-linear / diamond DAGs | ✅ | e2e `diamond_min` |
| `using (disabled = ref)` (self or call ref) | ✅ | e2e `modifiers_min` |
| `(preflight)` | ✅ gates the pipeline (early-abort): an input-bound preflight runs first and the pipeline's `pa` is gated on its completion, so every downstream call waits — mrp's prenode behavior. A preflight bound to a call output stays in DAG order (warns) | e2e `modifiers_min`, `kitchen_sink`; unit `TestEmitPreflightGate` |
| `(local)` | ⚠️ no-op (scheduling only) | e2e `kitchen_sink` |
| `(volatile)` / `volatile = strict` | ⚠️ no-op for outputs (VDR is 🚫) | e2e `kitchen_sink` |

## Map calls / forks — writing-stages / advanced-features

| Feature | Status | Test |
|---|---|---|
| `map call ... split` over an **array** → array result | ✅ | e2e `fork_min` |
| `map call ... split` over a **map** → keyed result | ✅ | e2e `map_fork`, unit `TestResolveForksMap/Merge` |
| fused multi-`split` (zipped) | ✅ | e2e `multisplit` |
| empty split collection → null result | ✅ | e2e `empty_fork_min` |
| map call over a **split stage** (fork-key threaded) | ✅ #16 | e2e `map_split` |
| map call over a **sub-pipeline** (per-fork body keying) | ✅ #16 | e2e `map_pipe`, `map_pipe_split` |
| map call emitting a **file** (array/keyed-map of files) | ✅ | e2e `map_file`, `map_file_keyed` |
| **complex combo**: map over a sub-pipeline whose body is a split stage emitting a file (Cell Ranger per-sample pattern) | ✅ | e2e + docker-iso `map_split_file` |
| **complex combo**: py + exec + comp adapters chained in one pipeline | ✅ | e2e `mixed_adapters` |
| **complex combo**: struct output whose field is a file array (struct walk → file[]) | ✅ | e2e + docker-iso `struct_file_array` |
| empty-split fork inside a map call (0 chunks → 0/null, no fork dropped) | ✅ | e2e `map_split` (includes an empty fork) |
| `disabled` on a map call | ✅ (fork pipeline-args gated by the resolved flag; skip → null bundle) | e2e `disabled_map` (skip true→null, false→[2,4,6]) |
| map call over a pipeline with an internal **disabled** call | ✅ (keyed pipeline gates the call per fork) | e2e `map_pipe_disabled` |
| **nested** map call (a map over a pipeline that itself maps) | ✅ (2-D fork keying: composite `outer~inner` keys, keyed FORKBIND/MERGE; the keyed FORKBIND enumerates per-fork bundle dirs from a staged `forknames.json` read via `.resolve()`, which is object-store-safe — `java.io.File.listFiles()` is **not** used) | e2e `map_pipe_nested`; live Batch + S3 (`map_pipe_nested`); unit guard `TestEmitDisabledNestedMap` (forbids `listFiles()`) |
| **disabled** nested map call (a disabled map nested inside an outer map) | ✅ (keyed disable gate: the flag is resolved per outer fork and either runs that fork's inner map or emits its null bundle) | e2e `map_pipe_disabled_nested` (fork0→[2,4], fork1 skip→null) |

## Stages / adapters / resources

| Feature | Status | Test |
|---|---|---|
| split / main / join | ✅ | e2e `split_test`, `kitchen_sink` |
| ordered chunk-outs aggregation in join | ✅ | shim `TestRunSumSquares`; `sort -V` |
| `py` adapter | ✅ | e2e (all py) |
| `exec` adapter | ✅ | e2e `exec_min` |
| `comp` adapter (mrjob-wrapped) | ✅ | unit `TestRunSumSquaresComp` |
| per-chunk resources `__mem_gb/__threads/__vmem_gb/__special` | ✅ in `_args`/`_jobinfo` | unit `TestMergeArgs*`, `TestSpecialResourcePreserved`, `TestJobInfoResolvedResources` |
| per-chunk resources → Nextflow scheduler directives | ✅ #15 (dynamic `cpus`/`memory` closures read the chunk's resolved resources; split & join phases also pass their resolved allocation to the shim so every phase's `_jobinfo` matches mrp) | e2e `split_test`, unit |
| negative/adaptive resource sentinels | ✅ preserved | unit `TestMergeArgsNegativeResources` |
| stage `using(mem_gb/threads)` incl. fractional → cpus/memory | ✅ | unit `TestEmitModules` (`memory '2 GB'`, `cpus 1`), shim |
| `using(vmem_gb)` enforcement | ✅ the declared value is passed to the shim via `-vmemgb` on every phase, so `mro2nf -monitor` caps the adapter's virtual memory (`prlimit --as`) at it and `_jobinfo` reports it; a per-chunk `__vmem_gb` overrides for main, a split-returned join `__vmem_gb` for join | unit `TestEmitVmemFlag`, `TestLimitedCommandMonitor` |
| `using(special)` → scheduler | ✅ mapped to a `clusterOptions` directive on every phase, looked up from `params.job_resources` (the `MRO_JOBRESOURCES` analog: a `special` string keys into a deploy-supplied map of scheduler options). Default map is empty (no-op on local; populate per deployment). A per-task `__special` (per-chunk / split-returned join) wins over the static key | e2e `special_resource`, unit `TestEmitSpecialScheduler`, `TestSpecialResourcePreserved` |
| split-returned `join` resource override → join phase directives | ✅ the split's `{"join": {...}}` block (`readStageDefs`) is emitted as `joinres.json` and read by the JOIN process's dynamic `cpus`/`memory`/`clusterOptions`, overlaying the stage's static `using()` field-by-field — matching mrp's `getJobReqs`. JOIN provisions and reports the overridden allocation | e2e `join_resources`, unit `TestReadStageDefsJoinOverride`, `TestEmitJoinResourceOverride` |
| `martian` module API (make_path, log_info/log_warn, update_progress, exit, alarm) | ✅ (real adapter drives it) | e2e `kitchen_sink`, `file_chain`, `api_smoke` |
| `martian.get_memory_allocation` / `get_threads_allocation` (read `_jobinfo`) | ✅ | e2e `api_smoke` (using(mem_gb=3,threads=2)→mem 3,threads 2) |
| directory-typed (`out path`) output published as a tree | ✅ | e2e `dir_out` (CopyTree dir recursion) |
| ASSERT vs retryable-error classification | ✅ shim exits 42 for an ASSERT (terminate) vs 1 for a retryable failure; config's dynamic `errorStrategy` routes accordingly | unit `TestStageFailureClassification`, `TestEmitConfig` |
| auto-adjust-memory / OOM escalation | ✅ memory directives grow with `task.attempt`, so an OOM-killed stage (a retryable failure) is retried with more memory instead of failing identically; cpus do not escalate. Attempt 1 is unchanged | unit `TestEmitModules` (`* task.attempt`) |
| `--monitor` vmem enforcement | ✅ with `mro2nf -monitor` (shim `prlimit --as` cap; RLIMIT_AS address-space, vs mrp's RSS poll) | unit `TestLimitedCommandMonitor` |

## Outputs / storage — storage-management

| Feature | Status | Test |
|---|---|---|
| file outputs published to results (mrp `outs/` layout, `GetOutFilename` naming) | ✅ | e2e `file_min`, `mrp_diff.sh` (tree diff vs real mrp) |
| inter-stage file passing (shared FS) | ✅ | e2e `file_chain` |
| inter-stage file passing (object store, no shared FS) | ✅ #13 (bundle-dir channels: files travel with their JSON as `@mre:file:` markers; the shim absolutizes on input, copies+relativizes on output) | e2e `cloud_sim`, `docker_iso`, unit `TestBundleRoundTrip` |
| auxiliary files reach isolated workers (types.json, bindspecs) | ✅ each task stages only the files it needs — the shared `types.json` plus, for a bind/fork process, its own `spec.json` — as individual `path` inputs, never referenced by `${projectDir}` (invisible on an AWS Batch / HealthOmics worker, which mounts only its work dir). A task transfers only its own bindspec, not every call's | e2e `docker_iso` (container that does not mount the project dir), unit `TestEmitAssetsStaged` |
| file-array / struct-of-file / map-of-file outputs publishing | ✅ #13 (PUBLISH walks the entry's output type, descending file-bearing structs) | e2e `map_file`, `map_file_keyed`, `struct_of_file`; unit `internal/types` |
| stage / pipeline `retain` | ⚠️ trivially satisfied (Nextflow keeps `work/`) | parse |
| VDR modes (disable/post/rolling/strict) timing | 🚫 no native dependency-gated mid-run deletion | — (terminal-state only) |
| `outs/` move+symlink layout | ⚠️ files are *copied* into an mrp-style `outs/` tree via `publishDir` (same layout and contents; mrp moves + symlinks) | e2e `file_min`, `mrp_diff.sh` |

## Running / inspecting — running-pipelines / inspecting-pipelines

| Feature | Status | Test |
|---|---|---|
| executor / jobmode (local + slurm/sge/lsf/pbs) | ✅ config profiles | unit `TestEmitConfig` |
| `--autoretry` (content-based retry) | ✅ dynamic `errorStrategy { exitStatus==42 ? terminate : retry }` + `maxRetries 2`; the shim's ASSERT exit code drives it | unit `TestEmitConfig`, `TestStageFailureClassification`; e2e `failure_paths` / Go `TestAssertTerminatesWithoutRetry`, `TestOrdinaryFailureRetriesWithEscalatedMemory` (assert terminates after 1 attempt with exit 42; ordinary failure retries and sees the escalated allocation) |
| cloud executors (awsbatch/k8s) | ✅ profiles emitted; #13 bundle data plane makes file flow object-store-correct; auxiliary files staged via `_assets` so isolated workers (no shared FS) can read them | config, e2e `cloud_sim` (copy-staging), `docker_iso` (true container isolation) |
| `-target awsbatch` (AWS Batch + S3) | ✅ Batch executor + classic aws-CLI S3 staging, in-container `/opt/mro2nf` paths, a generated `Dockerfile` + self-contained `runtime/` build context (bash/ps, no ENTRYPOINT, x86_64, aws CLI) | unit `TestEmitConfigTargets`, `TestEmitContainerBuild`; e2e `docker_iso` (built from the generated Dockerfile) |
| `-target healthomics` (AWS HealthOmics) | ✅ ECR-parameterized container, publishes to `/mnt/workflow/pubdir`, no executor (managed), pinned Nextflow version, `parameter-template.json` + `package.sh` (workflow zip) | unit `TestEmitConfigTargets`, `TestEmitHealthOmicsPackaging` |
| `-resume` ≈ restart-without-rerun | ⚠️ content-addressed cache (different mechanism) | e2e `runtime_knobs` / Go `TestResumeCachesEverything` (unchanged rerun: zero tasks re-execute) |
| `mrp --overrides` (per-stage resource retune at launch) | ✅ `mro2nf overrides` converts the JSON to a `process`/`withName:` `-c` overlay; or write it natively. See `RUNTIME_TUNING.md` | unit `TestConvert` |
| `--maxjobs` / `--jobinterval` / `--localcores` / `--localmem` (throttling) | ✅ Nextflow `executor.queueSize` / `submitRateLimit` / `cpus` / `memory`; documented in `RUNTIME_TUNING.md` | — |
| `--profile` / `--inspect` / `--onfinish` (profile, dry-run, hook) | ✅ Nextflow `-with-trace`/`-with-report`, `-preview`, `workflow.onComplete`; see `RUNTIME_TUNING.md` | — |
| web UI / HTTP API / mrstat | 🚫 use `nextflow log`/`-with-report` instead | — |
| `mro check` / `mro graph` as conformance oracle | ✅ used to validate every fixture; `mro graph` ≈ `nextflow run -with-dag` | `test/e2e`, `mro check` |

## Notes

- Every e2e fixture's expected output is the real `mrp` result, captured under
  `testdata/<name>/expected/`.
- Every Martian map-call combination is now supported, including the formerly
  guarded ones: a `disabled` map call, a map over a pipeline with an internal
  disabled call, and a nested map (a map over a pipeline that itself maps, via
  2-D composite-key fork threading). The transpiler no longer emits an
  `UnsupportedError` for any map-call shape. (The `lower` stage still guards a
  genuinely unknown expression kind as defensive code.)
- Former `mrp`-runtime-only items, now handled at the shim level (none affects
  output equivalence — final `results/` always match `mrp`):
  - **content-based retry (ASSERT vs retryable)** — ✅ done. The shim exits 42 for
    an `ASSERT:` failure and 1 otherwise; the config's dynamic `errorStrategy`
    terminates the former and retries the latter.
  - **`--monitor` vmem enforcement** — ✅ done (opt-in `mro2nf -monitor`). The shim
    caps the adapter via `prlimit --as`. Caveat: RLIMIT_AS bounds address space,
    where mrp polls RSS, so a high-virtual/low-resident stage may be capped
    sooner; enable only when you want that ceiling.
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
  scheduler, and every phase reports its resolved allocation in `_jobinfo`, so a
  transpiled run provisions like mrp and runs in a comparable duration. A
  purely split-returned `__special` is now routed even with no static key (the
  main/join phases read the per-task `__special`), and a declared `using(vmem_gb)`
  is passed to the shim via `-vmemgb` on every phase, so `--monitor` caps virtual
  memory (and `_jobinfo` reports it) at the declared value (a per-chunk
  `__vmem_gb` still wins for main; a split-returned join override refines it).
- Remaining ⚠️ items are storage-*mechanism* fidelity: the published `outs/`
  tree now reproduces mrp's nested layout and `GetOutFilename` naming
  (verified by the Go `TestMrpDiff` suite against real mrp), but files are copied
  into place rather than moved + symlinked, and `work/` is retained (no VDR) —
  no output-observable correctness impact.
