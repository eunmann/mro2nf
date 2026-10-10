# mro2nf

Transpiles Martian (`.mro`) pipelines into Nextflow projects. Generated Nextflow
orchestrates the DAG/splits/forks/resources; each process runs the **original**
Martian stage code through the Martian adapter ABI via the `mre` shim. See
`docs/TRANSPILER.md` for the end-to-end design.

## North star (the correctness + performance bar)

Faithfulness means exactly two contracts, nothing more:

1. **The Martian stage ABI**: each stage task receives `_args` and produces
   `_outs` in the byte shape the Martian adapter expects (`shim.WriteBundle`,
   `@mre:` markers). Never perturb this.
2. **Pipeline outputs**: a transpiled pipeline produces the **exact same
   outputs every run** as `mrp` (the golden + mrp-differential suites), and
   runs reliably and safely (deterministic ordering, `-resume`-stable,
   loud failures — never silent divergence).

Everything between stage tasks — binding, forking, gathering, disabling,
staging — is **not** a contract. Implement it as idiomatic, modern Nextflow
(channels, operators, native `path` staging), NOT as a byte-identical replica
of Martian's internal orchestration or of the current emitter's task graph.
100% Martian feature parity is not the goal; identical outputs are.

**Overhead rule**: orchestration cost (tasks, driver work, bytes staged, work
inside stage tasks that isn't the stage's own compute) must be O(pipeline
size + total data). It must never scale super-linearly with fork width N or
chunk count M, and per-fork/per-chunk bookkeeping tasks are a smell — the
split triad (split → per-chunk main → join, per fork) is the only intrinsic
fan-out; it matches Martian's own jobs 1:1. When judging a change, measure
tasks and per-instance work against `mrp`'s job count at two widths — `make
bench` does exactly this: the `bench/forks` and `bench/split` fixtures run at
two widths (default and `-native` lanes) and the gate fails if the plumbing
task count differs between widths. Constant overhead is acceptable, scaling
overhead is a bug.

## Architecture

```
cmd/mro2nf/            → transpiler CLI: .mro -> Nextflow project
                       (+ `overrides` subcommand: mrp --overrides -> -c config)
cmd/mre/             → runtime shim: runs stage phases (split|main|join)
                       against the real Martian adapter inside a NF process,
                       plus the data-plane subcommands the generated processes
                       call (bind|forkbind|merge|publish-layout|entryargs)
internal/
  frontend/          → parse .mro via github.com/martian-lang/martian/syntax
  ir/                → normalized transpiler IR (stages, pipelines, bindings)
  emit/              → IR -> Nextflow (.nf + nextflow.config) templates
  types/             → type-directed file-leaf walk (shared by emit + shim)
  shim/              → _args/_jobinfo/_outs I/O, path rewrite, adapter launch
  bind/              → resolve call bindings into _args (refs, projections, fan-in)
  overrides/         → mrp --overrides file -> Nextflow -c config
  config/            → .mro2nf.yml project defaults for the transpiler flags
  logging/           → zerolog setup (stderr)
  apperror/          → sentinel + typed errors
```

The Martian parser is consumed as a normal module dependency, pinned by commit
to public `github.com/martian-lang/martian` (see `go.mod`) — no `replace`, so a
fresh clone builds. To hack on the parser locally, add a gitignored `go.work`
(or a `replace`) pointing at a checkout. `martian-lsp` is a sibling reference for
`syntax` usage.

## Rules

- No behavior changes without tests. Run `make test` before committing.
- Never push broken builds. `make lint-check` must pass.
- Fix lint by improving the code — never `//nolint` unless provably the only
  option, with an inline comment explaining why.
- Maximum 70 lines per function (lint limit 80).
- Standard `testing` package only — no testify. Use `if` + `t.Errorf`/`t.Fatalf`;
  compare with `github.com/google/go-cmp`.
- Wrap errors with context: `fmt.Errorf("operation: %w", err)`.
- Keep `main` thin: parse flags, wire deps, delegate.
- Commit atomically: one logical change per commit, each building and passing
  `make test` on its own. Don't bundle unrelated fixes, refactors, and docs into
  one commit; split them so each can be reviewed and reverted independently.
  Write an imperative subject (`fix:`/`feat:`/`docs:`/`test:`/`refactor:`).
- Land changes via pull request, not direct pushes to `main`. Branch, push, open
  a PR, and let the PR Validation workflow (lint, build, unit tests, both e2e
  suites) go green before merging. Keep `main` releasable.

## Operating Principles

These override default agent behavior. They apply to the main agent and to every subagent — pass the relevant ones into subagent prompts verbatim.

### Conduct

- Do what is asked, fully. No partial scope, no deferrals, no excuses. When a genuine fork exists, present the options with evidence-backed tradeoffs and a recommendation, then do what the user picks.
- Be neutral, fair, and concise: facts, evidence, options, tradeoffs. No personality, no flattery, no hedging, no filler.
- Never make claims about the size, difficulty, or scope of work — not as a reason to do less, not as commentary, not as an estimate. Just do the work.
- No guesses or assumptions. Every claim about the codebase must be verified by reading code. Names, comments, and docs are claims, not proof. Cite `file:line`. A claim about what `mrp` does is verified against the pinned Martian source or a real `mrp` run.
- Challenge everything with first principles: the user's framing, the repo's existing patterns, prior memories, your own conclusions. If evidence contradicts the request, surface the evidence and the alternatives — then follow the user's decision.
- Sprawl before you edit: read the related features, consumers, and call paths so the change is correct in context, not just locally plausible.
- Experiment when evidence is cheaper to produce than to reason about: write a throwaway test, transpile a fixture and read the generated Nextflow, run it under Nextflow and `mrp`.

### Editing & Verification Discipline

- Use the Edit/Write tools for all source edits. Never `sed` or python heredocs to modify source — both silently no-op on anchor mismatch. CLI only where it is genuinely the better tool: moves, renames, bulk generation, data processing.
- Make all edits first, then run `make lint` once and tests once at the end of the change set — never mid-development. `make lint` covers govet and typecheck; never run `go vet` separately.
- Comments capture only a non-obvious *why*. Never narrate edits or restate code/config. Needing a comment is first a report about the code: rename, extract, or restructure until the code says it. Delete dead comments on sight, like dead code.

## Conventions

See `.claude/skills/{go-standards,testing,tdd,workflow,repo-research}` for the full rules.
Commands: `/issue` (implement an issue), `/review-loop` (bounded review before a PR),
`/create-pr`, `/address-pr-comments`, `/review-pr`, `/create-issue`, `/checklist`.
Run `make help` for targets.
