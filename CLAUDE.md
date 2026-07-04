# mro2nf

Transpiles Martian (`.mro`) pipelines into Nextflow projects. Generated Nextflow
orchestrates the DAG/splits/forks/resources; each process runs the **original**
Martian stage code through the Martian adapter ABI via the `mre` shim. See the
design plan at `~/.claude/plans/bright-booping-rose.md`.

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
tasks and per-instance work against `mrp`'s job count at two widths (see the
forks×chunks fixtures); constant overhead is acceptable, scaling overhead is
a bug.

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

## Conventions

See `.claude/skills/{go-standards,testing,tdd,workflow}` for the full rules.
Run `make help` for targets.
