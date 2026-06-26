# Martian feature coverage

This matrix tracks every Martian feature (from the martian-lang.org docs and the
authoritative `martian/syntax` grammar) against the transpiler's support and the
test that exercises it. Test types: **e2e** = transpile + run under Nextflow,
output diffed against real `mrp` (`test/e2e/run.sh`); **unit** = Go test;
**corpus** = parse+lower robustness sweep.

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
| literals: int/float/sci, bool, null, arrays, maps, struct literals, escapes | ✅ | `martian/syntax`; e2e entry args |
| bindings: `self.x`, `self.x.y`, `STAGE`, `STAGE.out`, `STAGE.out.field`, `STAGE.default` | ✅ | unit `bind`; e2e `struct_proj`, `default_out` |
| **field projection through arrays** (`CALL.s.field`) | ✅ | unit `TestResolveArrayProjection`, e2e `struct_proj` |
| field projection through maps (`map<S>.field`) | ⚠️ array done; map projection not yet | corpus (graceful) |
| wildcard binding `* = self` / `* = REF` | ✅ | e2e `wildcard` (compiler expands) |
| **refs nested in array/map literals** (`[A.x, B.x]` fan-in) | ❌ tracked #14 | corpus (clean `ErrUnsupported`) |
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
| map call over a **split stage / sub-pipeline** | ❌ tracked #16 | unit `TestEmitUnsupported` |
| `disabled` on a map call | ❌ tracked #16 | unit `TestEmitUnsupported` |

## Stages / adapters / resources

| Feature | Status | Test |
|---|---|---|
| split / main / join | ✅ | e2e `split_test`, `kitchen_sink` |
| ordered chunk-outs aggregation in join | ✅ | shim `TestRunSumSquares`; `sort -V` |
| `py` adapter | ✅ | e2e (all py) |
| `exec` adapter | ✅ | e2e `exec_min` |
| `comp` adapter (mrjob-wrapped) | ✅ | unit `TestRunSumSquaresComp` |
| per-chunk resources `__mem_gb/__threads/__vmem_gb/__special` | ✅ in `_args`/`_jobinfo` | unit `TestMergeArgs*`, `TestSpecialResourcePreserved`, `TestJobInfoResolvedResources` |
| per-chunk resources → Nextflow scheduler directives | ❌ tracked #15 (static cpus/memory today) | — |
| negative/adaptive resource sentinels | ✅ preserved | unit `TestMergeArgsNegativeResources` |
| stage `using(mem_gb/threads/vmem_gb/special)` incl. fractional | ✅ | config + shim tests |
| `martian` module API (make_path, log, progress, exit/throw, allocations) | ✅ (real adapter drives it) | e2e `kitchen_sink`, `file_chain` |
| ASSERT vs retryable-error classification | 🚫 mrp content-based retry | — (documented) |
| auto-adjust-memory / OOM escalation | 🚫 mrp runtime | — (documented) |
| `--monitor` vmem enforcement | 🚫 mrp/cgroup | — (documented) |

## Outputs / storage — storage-management

| Feature | Status | Test |
|---|---|---|
| file outputs published to results (path→basename rewrite) | ✅ | e2e `file_min` |
| inter-stage file passing (shared FS) | ✅ | e2e `file_chain` |
| inter-stage file passing (object store, no shared FS) | ❌ tracked #13 (M3b) | — |
| file-array / struct-of-file outputs publishing | ❌ tracked #13 | — |
| stage / pipeline `retain` | ⚠️ trivially satisfied (Nextflow keeps `work/`) | parse |
| VDR modes (disable/post/rolling/strict) timing | 🚫 no native dependency-gated mid-run deletion | — (terminal-state only) |
| `outs/` move+symlink layout | ⚠️ → `publishDir` (different mechanism, same result) | e2e `file_min` |

## Running / inspecting — running-pipelines / inspecting-pipelines

| Feature | Status | Test |
|---|---|---|
| executor / jobmode (local + slurm/sge/lsf/pbs) | ✅ config profiles | `nextflow.config` |
| cloud executors (awsbatch/k8s) | ⚠️ profiles emitted; needs #13 data plane | config |
| `-resume` ≈ restart-without-rerun | ⚠️ content-addressed cache (different mechanism) | — |
| web UI / HTTP API / mrstat | 🚫 use `nextflow log`/`-with-report` instead | — |
| `mro check` / `mro graph` as conformance oracle | ✅ used to validate every fixture | `test/e2e`, `mro check` |

## Notes

- Every e2e fixture's expected output is the real `mrp` result, captured under
  `testdata/<name>/expected/`.
- ❌ items fail fast with a typed `apperror.UnsupportedError` at transpile time;
  they never emit silently-wrong Nextflow. Tracked as tasks #13–#16.
- 🚫 items are `mrp`-runtime behaviors (live UI, content-based retry, mid-run VDR
  timing, OOM auto-escalation) with no faithful Nextflow analog; the closest
  testable guarantee (terminal filesystem state, output equivalence) is used
  where applicable.
