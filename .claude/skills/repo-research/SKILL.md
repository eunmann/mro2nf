---
name: repo-research
description: "Pre-implementation codebase research sized to the change: locate what it touches, read that, synthesize, plan. Use for new features, refactors, cross-cutting bug fixes and issue writing."
---

# Repo Research

Understand the blast radius of a change before making it, reading only what the change reaches. Skip this for typo fixes, one-line changes and mechanical edits; the calling command decides.

Every turn re-reads your whole context, so cost grows with what you load. Locate before you read, read in parallel batches, and delegate a subsystem only when its files would otherwise fill your context.

## 1. Orient

Read the sections of `docs/design.md` and the rows of `docs/feature-coverage.md` that cover the task, not all of them. The package map, for orientation:

- **Call graph:** `internal/cg` — Martian's compiled call graph: the witness, topology, plan, the `_args` resolver, fork elements, gates, pipeline outs
- **Emitter:** `internal/cgnf` — the call graph to a Nextflow project: `main.nf` wiring, `assets/stage.nf`, `nextflow.config`
- **Runtime (`mre`):** `cmd/mre` subcommands (`plan|stage|outs`), `internal/job` (one stage job through `mrjob`), `internal/shim` (bundles, markers, output confinement), `internal/types` (the file-leaf walk over Martian's compiled types)
- **Around it:** `cmd/mro2nf` (the transpile CLI: `-o`, `-mropath`, `-version`), `vendor-martian` (the pinned Martian adapters `mre` embeds)
- **Tests:** unit tests beside each package; `test/e2e` (build tag `e2e`: goldens, mrp-diff, bench, lint-nf); fixtures and real-`mrp` goldens in `testdata/<name>/`; `bench/`
- **Reference oracle:** the pinned Martian module (`go env GOMODCACHE`, version in `go.mod`) — `martian/syntax` for the language, `martian/core` for what `mrp` does at run time; a local `mrp` build at `$MARTIAN_BIN` (default `~/workdir/martian/bin`), which must be built at the commit `go.mod` pins — check `git -C ~/workdir/martian rev-parse --short HEAD` against it before trusting a differential.

**Lean on Martian; never drift from it.** `internal/cg` uses Martian's compiled call graph and exported API, and mirrors only the runtime pieces `core` does not export (fork expansion, `TopNode.resolve`, `Fork.disabled`, `ForkId.forkId`, merge `constrain`). A mirror follows the `core` function case for case over the same compiled structures, cites each case's `core/<file>.go:<line>` at the pinned commit, and changes only when that function does. Deriving behaviour from a heuristic of mro2nf's own over `sp.Value` instead of Martian's compiled `sp.Source` is how #595's fork-producer bugs arose; a heuristic that a `core` function already decides is the defect, not the fix.

## 2. Locate

Grep for the symbols, flags, record names, sidecar names and generated Nextflow constructs the task touches, and list each affected layer with one line on why. A change usually crosses a seam — `internal/cgnf` ↔ `mre` (flags, bundle shapes), `internal/cg` records ↔ the driver's Groovy helpers in `assets/stage.nf` (ordering, JSON), `internal/cg` ↔ `internal/job` — so find both sides.

Check, without reading them in full, whether the change needs: a new or re-recorded golden (record it from real `mrp`), a fixture in the e2e tables (`TestEveryFixtureIsExercised` enforces registration), a bench re-baseline (`BENCH_UPDATE=1 make bench` when task counts change), a target-specific path (awsbatch, healthomics), or a docs update (`docs/feature-coverage.md`, the `docs/design.md` section of the mechanism, the `docs/usage.md` section of a user-visible behavior).

## 3. Read

Read the affected code directly: the functions the change touches, their callers and their tests. Batch independent reads and greps into one turn. When the question is what the generated Nextflow looks like, transpile a fixture and read the output rather than reasoning from the generator.

Spawn an `Explore` subagent only for a subsystem whose files you would otherwise load just to answer one question: at most three, in parallel, each with that question and the instruction to return conclusions with `file:line`, not file contents.

## 4. Synthesize

- What does the code do today, as evidenced by `file:line`, against what the task assumes? What does `mrp` do (`martian/core`, or a real run)?
- Which existing pattern should the change follow, and where do features diverge?
- Is the stated approach right? Present a simpler or more robust alternative with its tradeoffs when there is one. Pre-production: prefer the clean replacement over a compatible patch.

## 5. Plan

List the commits in dependency order (call graph before emitter, `mre` flag before the generated script that passes it, type before callers), each with its files, change and tests. Mark which are independent.

For research-only commands (`/create-issue`), stop here.

## 6. Implement

Implement directly, or hand independent tasks to `implementer` subagents in parallel, each with the files, the behaviour, the tests and the commit split. Their output is verified by the gate and `/review-loop`, not by re-reading every diff.
