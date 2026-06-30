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
| 1 | `map<T[]>` and deeper (MapDim≥2) file/int leaves are silently never walked — staging, resolution, coercion, and publish all drop them | **CORRECTNESS (High)** | F (types) | fix |
| 2 | comp/exec (mrjob) backend never reads `_assert` → a compiled-stage assertion is a silent SUCCESS with stale outs | **CORRECTNESS (High, scoped)** | A, G | fix |
| 3 | negative-adaptive resource sentinels not resolved to `\|x\|` → adaptive stages under-provisioned; a stage can read a negative `get_memory_allocation()` | **CORRECTNESS (Medium)** | E | fix |
| 4 | empty array-mode map-call merge resolves to `null` instead of `[]`; but null-source and empty-array are already indistinguishable by merge time, and `null` is correct for the (common) null-source case | **CORRECTNESS (Medium)** | D | deferred |
| 5 | published file names/layout: mre used source basename + flat + numeric-suffix; Martian uses `GetOutFilename` + nested index/key subdirs | **CORRECTNESS** (was DESIGN) | C | fixed |
| 6 | `--monitor` enforces vmem (`RLIMIT_AS`) only, not mrp's primary RSS-vs-`mem_gb` kill | **DESIGN** | E | decision |
| 7 | `map<map<S>>.field` nested-map projection rejected at transpile time (loud) | MINOR (coverage) | D | document |
| 8 | over-retry: deterministic stage exceptions are retried (mrp fails them immediately) | MINOR | G | document |
| 9 | vmem cap not scaled on retry while mem is; join block accepts stray keys; `invocation` = stage args not top-level; version string `mro2nf`; `MRO_UUID` unset; empty-string output leaf → `""` not `null` | MINOR | A,C,E | document |

Items 5–6 are **design decisions** (the fixtures currently encode the mre
behavior), so they are surfaced for a product call rather than changed
unilaterally. Items 7–9 are minor/unreachable-for-valid-pipelines or intentional.

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

**Bug (CORRECTNESS, scoped):** `runWrappedAdapter` (the comp/exec path,
`shim.go:301-333`) reads only `_errors` + exit code — never `_assert`. With a real
mrjob, a compiled-stage assertion lands in `_assert` and mrjob exits 0, so mre
sees empty `_errors`/nil exit and returns the stale skeleton `_outs` as success.
**Fix:** read `_assert` in `runWrappedAdapter`; non-empty → `ErrStageAssert`.

**Intentional/minor:** `_complete`/`_perf`/heartbeat not written (NF replaces);
`monitor_flag` hardcoded `disable`; `invocation` carries stage args not the
top-level pipeline invocation; `version` = `mro2nf`; `MRO_UUID` unset.

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

**CORRECTNESS (#4, deferred — needs care):** an empty *array-mode* map-call
resolves to `null` (`bind.go mergeOne:446-448`) where Martian yields `[]` for an
empty array source (`resolve.go:391-403`); map-mode empties correctly yield `{}`.
But the fix is subtler than first reported: `buildArrayForks` coalesces a *null*
source to `[]` via `orEmptyArray` (`bind.go:139`), so by merge time an empty
present array and a null source are indistinguishable — both reach `mergeOne`
with `keys==nil` and zero forks. The current `null` result is therefore *correct*
for the null/disabled-upstream case (the common one, where Martian also yields
`null` via `ModeNullMapCall`) and wrong only for a genuinely empty present array.
A naive flip to `[]` would regress the null case. **Proper fix:** preserve the
null-vs-empty distinction end-to-end (don't coalesce null→`[]` in
`buildArrayForks`; carry a null-source marker, or the static `MapMode`, through
`forkkeys` into `merge`) so an empty array → `[]`, a null source → `null`, an
empty map → `{}`. Deferred because it touches the whole fork pipeline and risks
the currently-correct null path; tracked for a dedicated change.

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

**CORRECTNESS (#3):** negative-adaptive sentinels (Martian's "at least `|x|`")
are not resolved to `|x|`. NF `dynMem`/`dynCpus` gate on `> 0` so a negative
request falls back to the static default, and `coalesce` keeps the negative, so
`_jobinfo.memGB`/`__mem_gb` can be **negative** and `get_memory_allocation()`
returns a negative number. mrp's cluster path does `-res.MemGB`. **Fix:** take
`abs()` before provisioning and before writing `_jobinfo`/`__*`.

**DESIGN (#6):** `--monitor` caps address space (`RLIMIT_AS`≈`vmem_gb`) only;
mrp's *primary* monitor kill fires on RSS exceeding `mem_gb` (`mrjob.go`). A stage
whose RSS overruns `mem_gb` but whose address space stays under `vmem_gb` is
killed by mrp, not mre. Currently acknowledged only in a code comment.
**Decision:** document the analog, or add an RSS poller that kills at `mem_gb`.

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

**CORRECTNESS (#1, the highest-value finding):** Martian encodes `MapDim = 1 +
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
and struct fields of those.

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

1. **Fix #1 (MapDim walk):** correct `internal/types/types.go` `walk`/`coerce` and
   `internal/emit/generate.go` `fileFlattenExpr`; add the `map<T[]>` test matrix.
2. **Fix #2 (`_assert`):** read `_assert` in `runWrappedAdapter`.
3. **Fix #3 (negative sentinels):** `abs()` in resource resolution + directives.
4. **Fix #4 (empty array fork) — deferred:** preserve null-vs-empty through the
   fork pipeline (`buildArrayForks`→`forkkeys`→`merge`) so an empty array →`[]`,
   a null source →`null`. Not a naive flip; touches the whole pipeline.
5. **#5 (publish naming): done** — mre reproduces mrp's `outs/` tree (downstream
   pipelines consume it). **#6 (monitor RSS):** still a product call — add an
   RSS-vs-`mem_gb` kill to match mrp, or keep the vmem (`RLIMIT_AS`) cap.
6. **Document #7–#9** as known, bounded divergences.

Items 1–3 are unambiguous correctness fixes with no design tradeoff and landed
with regression tests (TDD). Item 4 is deferred: the correct behavior depends on
a null-vs-empty distinction that is currently collapsed, so it needs a scoped
change to the fork pipeline rather than a one-line flip. Items 5–6 need a
decision before any change, because the current behavior is deliberate and the
fixtures encode it.
