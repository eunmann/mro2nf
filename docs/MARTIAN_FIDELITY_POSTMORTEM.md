# Postmortem: how 7 Martian-fidelity bugs slipped past the test suite

End-to-end validation against a real CellRanger pipeline (`SC_RNA_COUNTER_CS`,
pbmc micro dataset) surfaced 7 distinct transpiler/runtime bugs that the unit
suite was fully green on. This document is the forensic half: **which parts of
Martian/MRP we failed to model, and why the tests we already had couldn't see
the gap.** The fixes themselves are the seven `fix:` commits on this branch; each
carries its own regression test.

The point is not "we needed more tests." It is that **the entire suite tested
mro2nf against its own assumptions, never against Martian's runtime contract.**
Three structural blind spots produced all seven bugs.

---

## The three blind spots

### Blind spot 1 — emit tests assert *text*, never *behavior*

`internal/emit/*_test.go` parse an MRO fixture, emit a Nextflow project into a
`t.TempDir()`, and `strings.Contains` the generated `.nf` against expected
snippets. **No test ever runs Nextflow or compiles the emitted Groovy.** So any
fault that lives in the *semantics* of the generated code — Nextflow's input
staging rules, Groovy's closure scoping, channel dataflow — is structurally
invisible. Three bugs lived exactly there.

| Bug | Martian/Nextflow behavior not modeled | Why the string-match test missed it |
|-----|----------------------------------------|--------------------------------------|
| **1. pipeargs aliases its `args` output** | Nextflow stages an unquoted `path pipeargs` under the channel item's *own basename*. A sub-pipeline's pipeargs **is** an upstream bind output — a dir named `args` — which is also this process's `-o args` output, so input and output collide. | The collision is a Nextflow *runtime* staging behavior; text match can't see it. Compounding it, no fixture nests pipeline-calls-pipeline, so the inner-pipeargs-is-`args` shape was never even emitted. The top-level entry uses `entry_resolved`, which never collides — so every fixture was in the safe case. |
| **2. nested file-flatten closures reuse `__e`** | Groovy rejects a nested closure that re-declares an enclosing closure's parameter. `file[][]` emits `.collect { __e -> ... .collect { __e -> ...} }`. | Every entry-file fixture had **at most one** array/map dimension over files, so only one `__e` was ever emitted — no shadowing. And the emitted Groovy is never compiled, so even a generated scope error wouldn't fail the test. |
| **5. zero-chunk split starves its JOIN** | `MAIN.out.collect()` on an empty Nextflow channel emits nothing, so JOIN never gets a complete tuple. Martian still runs the join with `_chunk_outs=[]` (`core/stage.go:1206-1263`). | `TestEmitJoinResourceOverride` **asserted the buggy `.collect()` string verbatim** — the test actively *protected* the bug. A string-match test can't observe that an empty channel drops JOIN at runtime, and no fixture drives a split that returns zero chunks. |

Bug 5 is the sharpest lesson in the set: a test that pins the current emitted
string is not a spec, it is a photograph of the implementation. When the
implementation is wrong, the test guards the wrong thing.

### Blind spot 2 — unit tests don't cross the serialization boundary where fidelity breaks

Bug **3** (empty `[]`/`{}` binding resolves to `null`) only exists *after* a JSON
round-trip: `valueToEntry` produced a non-nil empty `Entry{Array: []}`, and
`omitempty` on `bind.Entry` (`internal/bind/bind.go:52-60`) drops the empty slice
on marshal, so the reloaded all-nil Entry falls through `resolve` to `null`
(`bind.go:267`).

Martian keeps empty composites distinct from null at every layer
(`core/resolve.go:1157-1162`, `core/argument_map.go:379-384`,
`syntax/format_exp_json.go:210-216`): a nil slice marshals to `null`, a non-nil
empty one to `[]`, and both round-trip.

Why we missed it: `bind_test.go` builds `Entry` values **in Go** and calls
`Resolve` directly — never marshaling to disk and back. The emit tests `os.Stat`
the bindspec files but never read their JSON. So the one transform that matters
(emit → file → `mre bind`) was the one path with no coverage. **The bug lived in
the gap between two well-tested components, at the serialization seam neither
test crossed.**

### Blind spot 3 — the only end-to-end stage is too trivial to exercise the output ABI

The runtime suite (`shim_test.go`, `comp_test.go`) drives the real Martian
adapter, but only through one stage: `sum_squares`, whose outputs are **scalar
ints the stage always writes explicitly**. That single shape happens to dodge the
entire output-finalization contract, hiding three bugs.

| Bug | Martian behavior not modeled | Why `sum_squares` never triggered it |
|-----|------------------------------|--------------------------------------|
| **6. `_outs` skeleton is all-null** | Before a stage runs, Martian pre-fills each declared **file** output with a writable path `<files>/<name>.<ext>` (`makeOutArg`, `core/stage.go:44-67`; filename rule `GetOutFilename`, `syntax/struct_type.go:74-86`). Arrays→`[]`, maps→`{}`, structs/scalars→`null`. Stages write to / assert on those paths (`FILTER_BARCODES`: `assert outs.filtered_metrics_groups is not None`). | `sum_squares` has no file outputs — only scalar ints it sets directly. An all-null skeleton is indistinguishable from a pre-populated one when nothing reads a default output path. `writeSkeletonOuts` had **no unit test at all**. |
| **4. absent declared output aborts the bundle** | At stage finalize Martian keeps an unwritten file output's path string as-is (`core/stage.go:1286-1290`); `removeEmptyFileArgs` only edits VDR accounting, not `_outs`. A downstream join may need the path string. | Every bundle test writes the source file *first*, so `CopyTree`'s stat always succeeds. No test set a declared output to a path that was never written. |
| **7. absent output errors at publish** | Martian drops a never-written output from published outs, resolving it to `null` (`moveOutFile` Lstat→null, `core/post_process.go:407-411`). | No fixture publishes a pipeline whose final outs declare a file that was legitimately not produced. The `types` walk tests use transforms that never stat and never error, so "error aborts the whole walk" was never exercised — and no skip sentinel existed to express "drop this leaf." |

Note the asymmetry we also had to learn: Martian keeps the path at **finalize**
(bug 4) but nulls it at **publish** (bug 7). Two different stages of the same
lifecycle, two different correct behaviors. A suite that never ran a real
optional output couldn't have discovered either, let alone their distinction.

---

## What was actually "ignored" in Martian

Concretely, the controlling Martian source we had not modeled:

- `core/stage.go:44-67` — `makeOutArg`/`makeOutArgs`, output pre-population (bug 6).
- `syntax/struct_type.go:74-86` — `GetOutFilename`, the out-name/extension rule (bug 6).
- `core/stage.go:1277-1339` — `doComplete`; path kept verbatim at finalize (bug 4).
- `core/post_process.go:407-411` — `moveOutFile`, absent file → null at publish (bug 7).
- `core/stage.go:1123-1125,1206-1263` — zero-chunk split still runs join (bug 5).
- `core/resolve.go`, `core/argument_map.go`, `syntax/format_exp_json.go` — empty composite ≠ null (bug 3).

A subtle fidelity point we got *more* right than the original fix sketch: in
`makeOutArg`, scalar **directory** and **struct** outputs are pre-filled as
`null`, not as a path — despite a stale doc-comment on the function claiming
structs get a recursively-populated map. Our `ir.Param.IsFile` is a single bool
that is *also* true for structs-containing-files, so a naive "IsFile → path"
would over-populate. `Table.IsStruct` exists precisely to null those, matching
Martian's code (not its comment).

---

## Recommendations (ordered by leverage)

1. **Compile the emitted Groovy in emit tests.** A `nextflow -preview`/parse
   smoke check (or `groovy` compile) over at least one emitted project per
   fixture would have caught bug 2 outright and any future scope/syntax fault.
   Cheap, high-leverage, closes blind spot 1's worst case.
2. **Add an e2e fixture that actually runs Nextflow** over a nested,
   file-bearing pipeline. This is the only thing that catches staging-name
   collisions (bug 1) and empty-channel JOIN starvation (bug 5).
3. **Stop pinning emitted strings that encode behavior.** Assert the *property*
   ("the JOIN gather is guarded against an empty channel"), not the current
   substring. Bug 5's test encoded the defect; a property check would have failed
   when the behavior was wrong.
4. **Test across serialization seams.** Bindspec coverage must go emit → disk →
   `resolve`, not in-memory only. Any `omitempty`/optional field is a fidelity
   trap that only springs after a round-trip (bug 3).
5. **Grow the runtime fixtures past `sum_squares`.** Need stages with: a file
   output written to its pre-populated path (bug 6), an *optional* output the
   stage skips (bugs 4, 7), a struct/dir output (bug 6 null rule), and a split
   that returns zero chunks (bug 5).
6. **A Martian-conformance harness.** For representative stages, diff mre's
   `_outs` skeleton and published outs against *real* Martian on the same MRO.
   This turns "we think we match Martian" into a checked invariant — the thing
   the whole transpiler depends on and the thing no unit test was asserting.

The meta-lesson: a transpiler's correctness is defined by the *target runtime's*
behavior, so the test suite has to be anchored to that runtime — by executing the
generated code and by differential-testing against the reference — not to the
transpiler's own emitted artifacts.
