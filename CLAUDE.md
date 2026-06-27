# martian-nextflow

Transpiles Martian (`.mro`) pipelines into Nextflow projects. Generated Nextflow
orchestrates the DAG/splits/forks/resources; each process runs the **original**
Martian stage code through the Martian adapter ABI via the `mre` shim. See the
design plan at `~/.claude/plans/bright-booping-rose.md`.

## Architecture

```
cmd/mart/            → transpiler CLI: .mro -> Nextflow project
cmd/mre/             → runtime shim: runs one stage phase (split|main|join)
                       against the real Martian adapter inside a NF process
internal/
  frontend/          → parse .mro via github.com/martian-lang/martian/syntax
  ir/                → normalized transpiler IR (stages, pipelines, bindings)
  emit/              → IR -> Nextflow (.nf + nextflow.config) templates
  types/             → type-directed file-leaf walk (shared by emit + shim)
  shim/              → _args/_jobinfo/_outs I/O, path rewrite, adapter launch
  logging/           → zerolog setup (stderr)
  apperror/          → sentinel + typed errors
```

The Martian parser is consumed via a local `replace => ../martian` (the
aws-batch jobmode fork). `martian-lsp` is a sibling reference for `syntax` usage.

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

## Conventions

See `.claude/skills/{go-standards,testing,tdd,workflow}` for the full rules.
Run `make help` for targets.
