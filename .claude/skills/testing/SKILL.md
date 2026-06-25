---
name: testing
description: "Reference: Testing conventions, lint rules, and make targets. Consult when writing tests or debugging failures, not as an action to run."
---

# Testing Standards

## Conventions

- Every behavior change requires tests.
- Read existing test files in the package to learn the project's testing conventions before writing new tests.
- Use Go's standard `testing` package only. No third-party assertion libraries (`testify/assert`, etc.). Use `if` + `t.Errorf`/`t.Fatalf` directly.
- Use table-driven tests when multiple cases share the same test logic. Name every subtest clearly: `t.Run("negative_number", ...)`.
- Separate success-path and error-path tests into distinct test functions for clarity.
- Cover the positive space (valid inputs -> correct outputs) and the negative space (invalid inputs -> correct errors). Test edge cases: zero values, nil, boundaries, empty inputs.
- Compare structs with `google/go-cmp` (`cmp.Diff`), not `reflect.DeepEqual`.
- Do not test internal implementation details. Test the public API contract. (In-package tests are acceptable here for exercising unexported helpers — `testpackage` is excluded for `_test.go`.)
- Test files live next to the code they test: `parser.go` and `parser_test.go` in the same directory.
- No test should be skipped or commented out.

## Lint Rules That Affect Tests

- `mnd` linter catches magic numbers — use named constants or `//nolint:mnd` when a literal is clearer.
- `funlen` max 80 lines per function (applies to test helpers too).
- `gosec` is excluded for `_test.go` (tests legitimately launch subprocesses), as are `wrapcheck`, `unparam`, and `noctx`.

## Make Targets

Run these once at the **end** of a change set, after all edits are complete — never mid-development:

- `make lint` — run linter with auto-fix. If it auto-fixes files, stage and commit them. Covers govet and typecheck — never run `go vet` separately.
- `make lint-check` — linter without auto-fix; the CI zero-issue gate.
- `make test` — run unit tests (includes the stdio end-to-end handshake test). Run after lint is clean.
- `make test-nvim` — headless Neovim integration checks (requires `nvim` 0.12+). Run when changing LSP feature behavior.

Exception: when fixing a bug via the `tdd` skill, run the single reproducing test (`go test -run <TestName> ./<pkg>/`) during the red-green cycle; the full lint+test pass still happens once at the end.

## See Also

- `/go-standards` — full code quality rules
