# Martian feature coverage

This matrix tracks every Martian feature (from the martian-lang.org docs and the
authoritative `martian/syntax` grammar) against the transpiler's support and the
test that exercises it. Test types: **e2e** = transpile + run under Nextflow,
output diffed against real `mrp` (`test/e2e/run.sh`); **unit** = Go test;
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

## Language — writing-pipelines / language-details

| Feature | Status | Test |
|---|---|---|
| stage / pipeline / top-level call | ✅ | e2e `split_test`, all |
| top-level call targeting a bare **stage** | ✅ | e2e `stage_entry` |
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
| field projection through maps (`map<S>.field`) | ❌ guarded (the type-agnostic binder would return null; `validateProgram` rejects it with a typed type-resolving check) | unit `TestEmitUnsupported`, `testdata/unsupported/map_struct_proj` |
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
| `(preflight)` | ⚠️ runs as ordinary call (no early-abort gating) | e2e `modifiers_min` |
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
| empty-split fork inside a map call (0 chunks → 0/null, no fork dropped) | ✅ | e2e `map_split` (includes an empty fork) |
| map call over a pipeline with internal disabled/nested-map | ❌ guarded | unit `TestEmitUnsupported` |
| `disabled` on a map call | ❌ guarded | unit `TestEmitUnsupported` |

## Stages / adapters / resources

| Feature | Status | Test |
|---|---|---|
| split / main / join | ✅ | e2e `split_test`, `kitchen_sink` |
| ordered chunk-outs aggregation in join | ✅ | shim `TestRunSumSquares`; `sort -V` |
| `py` adapter | ✅ | e2e (all py) |
| `exec` adapter | ✅ | e2e `exec_min` |
| `comp` adapter (mrjob-wrapped) | ✅ | unit `TestRunSumSquaresComp` |
| per-chunk resources `__mem_gb/__threads/__vmem_gb/__special` | ✅ in `_args`/`_jobinfo` | unit `TestMergeArgs*`, `TestSpecialResourcePreserved`, `TestJobInfoResolvedResources` |
| per-chunk resources → Nextflow scheduler directives | ✅ #15 (dynamic `cpus`/`memory` closures read the chunk's resolved resources) | e2e `split_test`, unit |
| negative/adaptive resource sentinels | ✅ preserved | unit `TestMergeArgsNegativeResources` |
| stage `using(mem_gb/threads)` incl. fractional → cpus/memory | ✅ | unit `TestEmitModules` (`memory '2 GB'`, `cpus 1`), shim |
| `using(vmem_gb)` / `using(special)` → scheduler directive | ⚠️ carried in `_args`/`_jobinfo` but not mapped to a Nextflow directive (no native vmem ceiling; `special`→`clusterOptions` not emitted). Output-correct; allocation differs | unit `TestSpecialResourcePreserved` |
| split-returned `join` resource override → join phase directives | ⚠️ dropped (join uses the stage's static `using()` resources). Output-correct; allocation differs | — |
| `martian` module API (make_path, log_info/log_warn, update_progress, exit, alarm) | ✅ (real adapter drives it) | e2e `kitchen_sink`, `file_chain`, `api_smoke` |
| `martian.get_memory_allocation` / `get_threads_allocation` (read `_jobinfo`) | ✅ | e2e `api_smoke` (using(mem_gb=3,threads=2)→mem 3,threads 2) |
| directory-typed (`out path`) output published as a tree | ✅ | e2e `dir_out` (CopyTree dir recursion) |
| ASSERT vs retryable-error classification | 🚫 mrp content-based retry | — (documented) |
| auto-adjust-memory / OOM escalation | 🚫 mrp runtime | — (documented) |
| `--monitor` vmem enforcement | 🚫 mrp/cgroup | — (documented) |

## Outputs / storage — storage-management

| Feature | Status | Test |
|---|---|---|
| file outputs published to results (path→basename rewrite) | ✅ | e2e `file_min` |
| inter-stage file passing (shared FS) | ✅ | e2e `file_chain` |
| inter-stage file passing (object store, no shared FS) | ✅ #13 (bundle-dir channels: files travel with their JSON as `@mre:file:` markers; the shim absolutizes on input, copies+relativizes on output) | e2e `cloud_sim`, unit `TestBundleRoundTrip` |
| file-array / struct-of-file / map-of-file outputs publishing | ✅ #13 (PUBLISH walks the entry's output type, descending file-bearing structs) | e2e `map_file`, `map_file_keyed`, `struct_of_file`; unit `internal/types` |
| stage / pipeline `retain` | ⚠️ trivially satisfied (Nextflow keeps `work/`) | parse |
| VDR modes (disable/post/rolling/strict) timing | 🚫 no native dependency-gated mid-run deletion | — (terminal-state only) |
| `outs/` move+symlink layout | ⚠️ → `publishDir` (different mechanism, same result) | e2e `file_min` |

## Running / inspecting — running-pipelines / inspecting-pipelines

| Feature | Status | Test |
|---|---|---|
| executor / jobmode (local + slurm/sge/lsf/pbs) | ✅ config profiles | unit `TestEmitConfig` |
| `--autoretry` (coarse retry of failed tasks) | ⚠️ `errorStrategy 'retry'`/`maxRetries 2` (any-failure retry; mrp's content-based ASSERT-vs-retryable classification is 🚫) | unit `TestEmitConfig` |
| cloud executors (awsbatch/k8s) | ✅ profiles emitted; #13 bundle data plane makes file flow object-store-correct | config, e2e `cloud_sim` (copy-staging proxy) |
| `-resume` ≈ restart-without-rerun | ⚠️ content-addressed cache (different mechanism) | — |
| web UI / HTTP API / mrstat | 🚫 use `nextflow log`/`-with-report` instead | — |
| `mro check` / `mro graph` as conformance oracle | ✅ used to validate every fixture | `test/e2e`, `mro check` |

## Notes

- Every e2e fixture's expected output is the real `mrp` result, captured under
  `testdata/<name>/expected/`.
- ❌ items fail fast with a typed `apperror.UnsupportedError` at transpile time;
  they never emit silently-wrong Nextflow. The remaining ❌ are a `disabled`
  map call and a map over a pipeline whose body holds a disabled/nested-map call
  — both need the per-fork disable/nested-map threading layered on the keyed
  pipeline machinery.
- 🚫 items are `mrp`-runtime behaviors (live UI, content-based retry, mid-run VDR
  timing, OOM auto-escalation) with no faithful Nextflow analog; the closest
  testable guarantee (terminal filesystem state, output equivalence) is used
  where applicable.
- Field projection through a *typed map* (`map<S>.field`) is now **guarded** with
  a typed `UnsupportedError` (it never emits silently-wrong output); projection
  through arrays and structs is fully supported. Enabling it would require
  threading per-segment type info into the (currently type-agnostic) binder.
- Remaining ⚠️ items are resource-*allocation* fidelity (vmem/special directives,
  split-returned join overrides) — outputs are identical to mrp; only the
  requested scheduler resources differ — and storage-*layout* fidelity (flat
  `publishDir` results vs mrp's nested `outs/` tree; published names use the
  written basename, with collisions disambiguated, rather than mrp's
  `GetOutFilename`). All have no output-observable correctness impact.
