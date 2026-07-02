# Martian runtime-contract conformance audit

A systematic audit of whether mro2nf reproduces Martian/MRP's runtime contract,
covering seven surface areas. Each area was cross-checked against the Martian
source (`github.com/martian-lang/martian@4a558e7`) and the mro2nf implementation,
with citations.

## Scope and the compatibility boundary

mro2nf is a transpiler: Nextflow orchestrates the DAG, and each NF process runs
**original** Martian stage code through the real Martian adapter via the `mre`
shim. mro2nf therefore deliberately does **not** reimplement mrp's pipestance
orchestration, VDR garbage collection, `-resume` bookkeeping, heartbeat, or perf
accounting — Nextflow replaces those. "100% compatible" is judged at the boundary
each stage process actually crosses: the **adapter + metadata + type/binding/
resource ABI**. Divergences below are tagged accordingly:

- **CORRECTNESS** — a real bug: wrong/lost data or a silent false success.
- **DESIGN** — a deliberate behavioral choice that diverges from mrp; needs a
  product decision, not obviously a bug.
- **INTENTIONAL** — mrp behavior that Nextflow legitimately replaces.
- **MINOR** — cosmetic or unreachable for compiler-valid pipelines.

## Verdict

The core contract is reproduced faithfully: the metadata file ABI, adapter
phase-invocation (Python path), split→main→join and fork/merge lifecycle,
value semantics (empty `[]`/`{}` vs null, float fidelity, int coercion, null
propagation), resource keys/overlay/defaults, disabled-call null shaping, and
preflight gating all match Martian. **Three genuine correctness bugs** and a
small number of design/minor divergences remain. Ranked:

| # | Finding | Class | Area | Status |
|---|---------|-------|------|--------|
| 1 | `map<T[]>` and deeper (MapDim≥2) file/int leaves are silently never walked — staging, resolution, coercion, and publish all drop them | **CORRECTNESS (High)** | F (types) | fixed |
| 2 | comp/exec (mrjob) backend never reads `_assert` → a compiled-stage assertion is a silent SUCCESS with stale outs | **CORRECTNESS (High, scoped)** | A, G | fixed |
| 3 | negative-adaptive resource sentinels not resolved to `\|x\|` → adaptive stages under-provisioned; a stage can read a negative `get_memory_allocation()` | **CORRECTNESS (Medium)** | E | fixed |
| 4 | empty/zero-fork map call: runtime empty/null source now yields the typed empty (`[]`/`{}`) matching mrp via static `MapMode`; literal-empty cases give typed empty vs mrp `null` (justified — see below) | **CORRECTNESS (Medium)** | D | fixed (runtime); justified residual (literal) |
| 5 | published file names/layout: mre used source basename + flat + numeric-suffix; Martian uses `GetOutFilename` + nested index/key subdirs | **CORRECTNESS** (was DESIGN) | C | fixed |
| 6 | `--monitor` enforced vmem (`RLIMIT_AS`) only, not mrp's primary RSS-vs-`mem_gb` kill | **CORRECTNESS** (was DESIGN) | E | fixed |
| 7 | `map<map<S>>.field` nested-map projection rejected at transpile time (loud) | MINOR (coverage) | D | document |
| 8 | over-retry: deterministic stage exceptions are retried (mrp fails them immediately) | MINOR | G | document |
| 9 | vmem cap not scaled on retry while mem is; join block accepts stray keys; `invocation` = stage args not top-level; version string `mro2nf`; `MRO_UUID` unset; empty-string output leaf → `""` not `null` | MINOR | A,C,E | document |

Items 5–6 were initially raised as design decisions; both were confirmed as
requirements and fixed (publish tree consumed downstream; RSS kill is mrp's
primary monitor). Items 7–9 are minor/unreachable-for-valid-pipelines or
intentional.

---

## A. Metadata ABI & adapter protocol — faithful (one scoped bug)

**Contract.** Metadata files are `_`-prefixed (`core/metadata.go:66`). The adapter
is launched (via `mrjob`) as `… martian_shell.py <stagecode> <phase> <meta>
<files> <journal>`; phase dispatch reads `_args`, requires `_outs` for main/join,
writes `_stage_defs`/`_outs` (`martian_shell.py:591-623`). The Python adapter
reads exactly six `_jobinfo` keys (`profile_mode`, `stackvars_flag`, `invocation`,
`version`, `threads`, `memGB`; `martian_shell.py:360-369`) — no `vmemGB` getter.
mrjob demuxes fd 4: an `ASSERT:`-prefixed message → `_assert`, else `_errors`,
then exits 0 (`cmd/mrjob/mrjob.go:222-243`).

**mro2nf.** `internal/shim/shim.go` reproduces argv, fd 3 (`_log`) / fd 4 (errors)
on the Python path, all six `_jobinfo` keys, the mandatory `_outs` skeleton, and
`_chunk_defs` with resources stripped for join. Python-path asserts (`ASSERT:` on
fd 4) → `ErrStageAssert` → exit 42. Faithful.

**Bug (CORRECTNESS, scoped) — FIXED:** `runWrappedAdapter` (the comp/exec path)
originally read only `_errors` + exit code — never `_assert` — so a real-mrjob
assertion (written to `_assert`, exit 0) was silently treated as success. It now
reads `_assert` first and classifies it as `ErrStageAssert`
(`shim.go` runWrappedAdapter; TestWrappedAdapterReadsAssert).

**TMPDIR — FIXED:** mrp creates `<meta>/tmp` and sets `TMPDIR` per job
(`core/node.go`, `metadata.go` TempDir) so stage tempfiles land on the job's
scratch volume. mre originally inherited the task env; it now creates the tmp
dir in `prepDirs` and sets `TMPDIR` on the adapter child (`adapterEnv`;
TestAdapterTMPDIR). On container backends this keeps `tempfile` off the small
root volume.

**Intentional/minor:** `_complete`/`_perf`/heartbeat not written (NF replaces);
`monitor_flag`, `profile_mode`, and `stackvars_flag` hardcoded `disable` (no
mrp `--profile`/`--stackvars` analogs); `invocation` carries stage args not the
top-level pipeline invocation; `version` = `mro2nf`; `MRO_UUID` unset; mre reads
the whole fd-4 error payload where mrjob truncates at 8100 bytes (mre is the
more permissive side); mrjob sets PDEATHSIG on its child while mre relies on
Nextflow's task-tree kill (an orphaned stage can briefly outlive a SIGKILLed
mre — accepted, as NF owns the supervision tree).

## B. Lifecycle & forks — faithful

split→main→join, chunk ordering (recovered via `sort -V`, resources stripped for
join), the zero-chunk join (`.ifEmpty([])` on the non-keyed path and remainder-
joins on both keyed/mapped paths — verified complete across all four variants),
no-split chunk[0]-forward, key-preserving fork merges, and disabled-call
null-forwarding all match Martian (`core/stage.go`, `fork.go`). Internal dir
naming (`chunk_%05d`, `fork_%05d`) differs but is never observed by stage code.
**Watch item:** a single map-call sweeping two *independent* sources is zipped
element-wise (length/key-mismatch errors), not cartesian — matches Martian's
nested-fork model but warrants a confirmatory test.

## C. Output handling & publish

**Faithful:** pre-population (`makeOutArg`: array→`[]`, map→`{}`, struct/scalar→
`null`, scalar file→`<files>/<GetOutFilename>`), the filename rule (`OutName`,
else bare name for builtin `file`/`path`, else `name.<typename>`), chunk-out
pre-population, finalization keeping an absent file's path verbatim, and publish
nulling an absent file. The scalar-directory worry is a non-issue: the only
scalar directory kind is a struct, which `IsStruct` already nulls.

**FIXED (#5):** Martian publishes final files under `GetOutFilename`, nesting
arrays/maps/structs into index/key/field-named subdirectories
(`post_process.go:237-486`). mre previously published flat under the source
basename. Because downstream pipelines consume an upstream's `outs/` tree, this
is a correctness requirement, not a preference. `cmd/mre/publish.go` now
reproduces mrp's layout exactly: a scalar named by `GetOutFilename` (`OutName`,
else bare name for builtin file/path or complex, else `name.<typename>`); an
array → `<param>/<idx>.<ext>` with index width = digits of the element count
(Martian's `WidthForInt`); a typed map → `<param>/<key>.<ext>` (illegal Unix
filenames skipped); a struct → `<param>/<field>` recursed by each field's
`GetOutFilename`; missing/empty leaves → `null`. The one deliberate departure:
the JSON leaf value is the path **relative to the outs dir** (e.g.
`shards/0.csv`) rather than mrp's absolute path, preserving mro2nf's
location-independent bundles while matching the on-disk tree. Five goldens were
updated to the tree layout.

## D. Binding & resolution — faithful (two real divergences)

**Faithful:** ref/struct/array/single-map projection, `array<map<S>>.field`
(MapInArray), empty `[]`/`{}` literal encoding across all binding paths, `42.0`
float fidelity, `5.0`→`5` int coercion, null propagation, and lexical key
ordering all match Martian (`core/resolve.go`, `argument_map.go`).

**#4 — mostly FIXED (runtime cases); one justified residual (literal cases).**
An empty/zero-fork map call's result depends on whether the source is a
*statically-known-empty literal* or a *runtime* (typed stage-output) value.
Verified against real `mrp` and the transpiled Nextflow across the full matrix
(`test/e2e/mapcall_matrix.sh`, which now includes null-source cases):

| map-call source | Martian (mrp) | mro2nf (after fix) |
|-----------------|---------------|--------------------|
| runtime-empty array (typed) | `[]` | `[]` ✓ |
| runtime-null array (`float[]`=None) | `[]` | `[]` ✓ |
| runtime-empty map (typed) | `{}` | `{}` ✓ |
| runtime-null map (`map<float>`=None) | `{}` | `{}` ✓ |
| non-empty array / map | `[…]` / `{…}` | same ✓ |
| literal empty array `[]` | `null` | `[]` (justified) |
| literal empty map `{}` | `null` | `{}` (justified) |

Martian dispatches on the source's static `CallMode` plus `KnownLength`: a
statically-known-empty **literal** → `ModeNullMapCall` → `null` (collapsed at
compile time by `MergeExp.BindingPath`, `merge_exp.go:389-427`), while a
**runtime** source of unknown length keeps its typed `ModeArrayCall`/`ModeMapCall`
and yields the typed empty (`[]`/`{}`) via `resolveMerge`'s `emptyFork` branch
(`resolve.go:389-403`) — *regardless of whether the runtime value is empty or
null*, because the mode comes from the declared type.

**Fix (implemented):** mre now decides array-vs-map from the **static** `MapMode`
(threaded via `mre forkbind -mapmode` from `ir.Call.MapMode`) instead of sniffing
the runtime JSON — so a null `map<T>` source forks as a map and merges to `{}`
(previously null), and any empty/null array source merges to `[]` (`bind.go`
`ResolveForks`/`mergeOne`). All six **runtime** cases now match mrp exactly.

**Residual (justified, not a bug):** the two literal cases give the typed empty
where mrp gives `null`. Matching mrp there would require compile-time whole-
program `KnownLength` propagation (reimplementing Martian's static fork pruning),
and — for the only realistic form, an entry input defaulting to `[]` — it would
**break mro2nf's launch-override feature** (statically pruning the map call to
`null` would ignore a launch-time `v=[1,2]`). mro2nf's runtime-generic model
(entry inputs overridable) makes the typed empty the correct behavior for it. See
`testdata/empty_fork_min` (literal) vs `testdata/map_null_map` (runtime, matches
mrp).

**MINOR/coverage (#7):** Martian's recursive `resolvePath` supports
`map<map<S>>.field`; mro2nf rejects it loudly at transpile time
(`checkSupported`). (Auditors disagreed on whether Martian's *syntax* forbids this
projection — `struct_type.go` has an "invalid projection through nested maps"
guard — so the gap may be narrower than it looks. Loud rejection is safe either
way.) Missing-struct-key→null and fractional-float-for-int leniency are
unreachable for compiler-valid pipelines.

## E. Resource model — faithful core (two real gaps)

**Faithful:** `__threads`/`__mem_gb`/`__vmem_gb`/`__special` key names and the
`0`-unset convention, re-injection into `_args`, chunk/join overlay precedence,
`vmem = mem + 3` default (`extraVMemGB`), the `_jobinfo` fields the adapter reads,
the resolved (not raw) allocation reported to `get_*`, and `special`→
`clusterOptions`/`params.job_resources` as the `MRO_JOBRESOURCES` analog.

**CORRECTNESS (#3) — FIXED:** negative-adaptive sentinels (Martian's "at least
`|x|`") were not resolved to `|x|`: NF `dynMem`/`dynCpus` gated on `> 0` so a
negative request fell back to the static default, and `coalesce` kept the
negative, so `_jobinfo.memGB`/`__mem_gb` could be **negative** and
`get_memory_allocation()` returned a negative number (mrp's cluster path does
`-res.MemGB`). Fixed by taking the absolute value on both sides: `absResource`
in the shim's resource merge (`internal/shim/meta.go`) and `Math.abs` in the
emitted dynamic `cpus`/`memory` directives.

**FIXED (#6):** `--monitor` previously capped address space (`RLIMIT_AS`≈`vmem_gb`)
only; mrp's *primary* monitor kill fires on RSS exceeding `mem_gb` (`mrjob.go`).
`internal/shim/monitor.go` now adds that RSS kill: the adapter runs as a process-
group leader, and a monitor samples the group's resident memory every second
(summed from `/proc/<pid>/statm` over the group, like mrp's mrjob) and SIGKILLs
the group when it exceeds `mem_gb`, writing mrp's quota message to `_errors`. The
kill is not an ASSERT, so it stays retryable — Nextflow retries with the escalated
memory (`memory * task.attempt`). The `RLIMIT_AS`/`vmem_gb` cap still applies as
the hard address-space bound.

**Minor/intentional:** linear OOM escalation (`mem * task.attempt`, always on) vs
mrp's `--auto-adjust-memory` RSS formula; vmem cap not scaled on retry; GPU
(`special="gpu:N"`→`accelerator`) is an mro2nf extension; mem→threads packing
left to the executor. All documented design choices.

## F. Types & validation — one High-severity bug

**Faithful:** struct outputs nulled (`IsStruct`) and descended regardless of
`IsFile`; `outFilename` == `GetOutFilename`; scalar/array/`map<T>` int coercion
(with a benign `≥2^63` edge); null/absent output tolerance == `ValidateOutputs`.
The file-vs-directory `IsFile`-bool collapse is **benign** (pre-population is
structurally guarded; leaf copy stats at runtime; staging/publish decompose to
scalar `KindIsFile` leaves).

**CORRECTNESS (#1, the highest-value finding) — FIXED:** Martian encodes `MapDim = 1 +
innerArrayDim` — a typed map is exactly one map level whose element is an array of
dim `MapDim-1` (`syntax/collection_types.go:325-331`; confirmed in
`internal/frontend/lower.go:342-347`). The shared walk (`internal/types/types.go`)
instead treats `MapDim` as a count of nested map levels and only spends array
traversal from `arrayDim`. So `map<file[]>` lowers to `{MapDim:2, ArrayDim:0}`,
the inner array hits `if arrayDim<=0 { return v }` and is returned **unwalked** —
0 of 2 file leaves staged/copied/published, silently. This breaks input staging,
emit-path resolution, int coercion, AND publish for `map<T[]>` and deeper,
including such types nested in structs. The dispatch is faithful only for
`MapDim≤1` and arrays-of-maps (`map<T>[]` = `{ArrayDim:1, MapDim:1}`); the
existing "map of file array" test uses `(1,1)`, which is actually `map<T>[]`, so
the true `map<T[]>` shape was never covered. The same root cause affects the
emit-side `fileFlattenExpr`. **Fix:** model `MapDim` as one map level carrying
`MapDim-1` inner array dims (on entering a typed map, descend one level then treat
the element with `arrayDim += mapDim-1, mapDim=0`) in `walk`/`walkMap`, `coerce`,
and `fileFlattenExpr`; add tests for `map<file[]>`, `map<int[]>`, `map<file[][]>`,
and struct fields of those. **This fix landed** — `internal/types/types.go` and
the emit-side `fileFlattenExpr` now model one typed-map level carrying
`MapDim-1` inner array dims, with the `map<T[]>` test matrix
(`internal/emit/mapdepth_test.go`, `internal/types`) and the `map_file_array`
fixture as regression cover.

## G. Errors, retries, preflight, disabled — faithful (Python path)

**Faithful:** the adapter's fd-4/`exit 0` contract is correctly inverted to a
non-zero process exit on the Python path; `ASSERT:`→exit 42→NF `terminate`
matches "assert never retries"; ordinary/signal/OOM→exit 1→NF `retry`
(`maxRetries=2` == `default_retries`); disabled-call out shaping is exact (each
top-level out param = JSON null, matching `makeOutArgs(nullAll=true)`); preflight
gating reproduces early-abort/validation semantics.

**CORRECTNESS:** see #2 (comp/exec `_assert`). **MINOR:** mre retries deterministic
exceptions that mrp fails immediately (same final outcome, wasted compute);
only pipeline-input-bound preflights gate the rest of the pipeline.

---

## Recommended actions

1. **#1 (MapDim walk): done** — `internal/types/types.go` `walk`/`coerce` and
   `internal/emit/generate.go` `fileFlattenExpr` corrected, with the `map<T[]>`
   test matrix.
2. **#2 (`_assert`): done** — `runWrappedAdapter` reads `_assert` first.
3. **#3 (negative sentinels): done** — `abs()` in resource resolution + directives.
4. **#4 (empty/zero-fork map call): done for runtime:** mre decides
   array-vs-map from the static `MapMode` (via `mre forkbind -mapmode`), so every
   runtime empty/null source matches mrp. The literal cases are a justified
   residual (matching would break entry-override). Validate with
   `test/e2e/mapcall_matrix.sh`.
5. **#5 (publish naming): done** — mre reproduces mrp's `outs/` tree (downstream
   pipelines consume it). **#6 (monitor RSS): done** — the RSS-vs-`mem_gb` kill is
   implemented alongside the `RLIMIT_AS` vmem cap.
6. **Document #7–#9** as known, bounded divergences.

Items 1–3 were unambiguous correctness fixes with no design tradeoff and landed
with regression tests (TDD). Item 4 landed for every runtime case; the two
literal-empty cases are a justified residual (matching mrp there would break the
launch-override feature — see area D). Items 5–6 were confirmed as requirements
and fixed. Only 7–9 remain, as documented, bounded divergences.

## Differential testing against real Martian

`make test-mrp-diff` (the Go `TestMrpDiff` suite) runs each fixture through **real
`mrp`** and the transpiled Nextflow, then compares the published `outs/` tree —
every output file's path (relative to the outs dir) and its content hash — and
the path-normalized `_outs` JSON. This validates the whole transpile + runtime +
publish path against Martian itself, not a hand-written golden.

It needs a local Martian build: set `MARTIAN_BIN` (default `~/workdir/martian/bin`,
which holds `mrp`/`mrjob`). It skips cleanly when `mrp`, `nextflow`, `java`, or
`python3` is absent, so CI without a Martian checkout is unaffected. Run one case
with `go test -tags e2e -count=1 -run "TestMrpDiff/<fixture>" ./test/e2e/`.

The one intended difference the harness normalizes: Martian writes **absolute**
paths into `_outs`; mre writes the **same tree** with paths **relative** to the
outs dir (location-independent bundles). Tree structure, filenames, and contents
are byte-identical.

Fixtures that read a launch-time file input (relative-path or `-params`) or that
the local `mrp` build can't run standalone (e.g. a nested map pipeline binding
this `mrp` version rejects) are covered by the golden e2e (`make test-e2e`) but
excluded from the mrp differential. `testdata/file_tree` is the flagship complex
case: a split→join file array, a nested sub-pipeline whose map call emits a
per-fork file, and a struct output bundling an array, a map, and a scalar file —
9 published files, all matching `mrp`.

**VDR note:** Martian garbage-collects an intermediate file even when it is also a
pipeline output (it publishes such a value as `null`); mro2nf does not implement
VDR (Nextflow owns file lifecycle), so it keeps the file. Fixtures used for the
differential therefore return final-only file outputs, isolating the publish-tree
contract from file-lifecycle policy.
