---
name: repo-research
description: "Pre-implementation codebase research sized to the change: locate what it touches, read that, synthesize, plan. Use for new features, refactors, cross-cutting bug fixes and issue writing."
---

# Repo Research

Understand the blast radius of a change before making it, reading only what the change reaches. Skip this for typo fixes, one-line changes and mechanical edits; the calling command decides.

Every turn re-reads your whole context, so cost grows with what you load. Locate before you read, read in parallel batches, and delegate a subsystem only when its files would otherwise fill your context.

## 1. Orient

Read the parts of `docs/TRANSPILER.md` and `docs/FEATURE_COVERAGE.md` that cover the task, not all of them. The package map, for orientation:

- **Front end:** `internal/frontend` (parse via `martian/syntax`, lower), `internal/ir`
- **Emitter:** `internal/emit` — `plan.go` decides each call's kind once; `generate*.go` render processes and wiring; `inline.go`; `diagnostics.go`; `lib/Mro2nf.groovy` (driver-side helpers); `runner/` (the `-native-runner` Python runner)
- **Runtime (`mre`):** `cmd/mre` subcommands (`split|main|join|bind|forkbind|merge|entryargs|publish-layout`), `internal/shim` (bundles, markers, adapter launch, `_jobinfo`, monitor), `internal/bind` (binding resolution), `internal/types` (type-directed file-leaf walk and the `types.json` manifest)
- **Around it:** `internal/overrides` + `mro2nf overrides`, `internal/config` (`.mro2nf.yml`), `cmd/mro2nf`
- **Tests:** unit tests beside each package; `test/e2e` (build tag `e2e`: goldens, mrp-diff, docker, bench, lint-nf); fixtures and real-`mrp` goldens in `testdata/<name>/`; `bench/`
- **Reference oracle:** the pinned Martian module (`go env GOMODCACHE`, version in `go.mod`) — `martian/syntax` for the language, `martian/core` for what `mrp` does at run time; a local `mrp` build at `$MARTIAN_BIN` (default `~/workdir/martian/bin`)

## 2. Locate

Grep for the symbols, flags, bindspec fields, sidecar names and generated Nextflow constructs the task touches, and list each affected layer with one line on why. A change usually crosses a seam — emitter ↔ `mre` (flags, bindspecs, bundle shapes), Go walk ↔ `lib/Mro2nf.groovy` (ordering, JSON), plain ↔ keyed emitters — so find both sides.

Check, without reading them in full, whether the change needs: a new or re-recorded golden (record it from real `mrp`), a fixture in the e2e tables (`TestEveryFixtureIsExercised` enforces registration), a bench re-baseline (`BENCH_UPDATE=1 make bench` when task counts change), a target-specific path (awsbatch, healthomics), or a docs update (`FEATURE_COVERAGE.md`, `TRANSPILER.md`).

## 3. Read

Read the affected code directly: the functions the change touches, their callers and their tests. Batch independent reads and greps into one turn. When the question is what the generated Nextflow looks like, transpile a fixture and read the output rather than reasoning from the generator.

Spawn an `Explore` subagent only for a subsystem whose files you would otherwise load just to answer one question: at most three, in parallel, each with that question and the instruction to return conclusions with `file:line`, not file contents.

## 4. Synthesize

- What does the code do today, as evidenced by `file:line`, against what the task assumes? What does `mrp` do (`martian/core`, or a real run)?
- Which existing pattern should the change follow, and where do features diverge?
- Is the stated approach right? Present a simpler or more robust alternative with its tradeoffs when there is one. Pre-production: prefer the clean replacement over a compatible patch.

## 5. Plan

List the commits in dependency order (IR before emitter, `mre` flag before the generated script that passes it, type before callers), each with its files, change and tests. Mark which are independent.

For research-only commands (`/create-issue`), stop here.

## 6. Implement

Implement directly, or hand independent tasks to `implementer` subagents in parallel, each with the files, the behaviour, the tests and the commit split. Their output is verified by the gate and `/review-loop`, not by re-reading every diff.
