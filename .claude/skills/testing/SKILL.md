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
- Assert the *property*, not the current emitted string. A test that pins generated text verbatim is a photograph of the implementation — when the implementation is wrong, the test guards the bug (this happened: a pinned `.collect()` string protected a zero-chunk JOIN starvation).
- Test across serialization seams. A value built in Go and consumed in Go skips the marshal→disk→unmarshal round-trip where `omitempty` and null-vs-empty fidelity break; cover the emit → file → consume path, not just in-memory calls.

## Lint Rules That Affect Tests

- `mnd` linter catches magic numbers — use named constants or `//nolint:mnd` when a literal is clearer.
- `funlen` is **excluded for `_test.go`**: a table-driven test's case list is data, not procedural code, so a long table is fine — keep cases in one table rather than splitting them artificially to satisfy a length limit. (Production code is still capped at 80 lines.)
- `gosec` is excluded for `_test.go` (tests legitimately launch subprocesses), as are `wrapcheck`, `unparam`, and `noctx`.

## Make Targets

Run these once at the **end** of a change set, after all edits are complete — never mid-development:

- `make lint` — run linter with auto-fix. If it auto-fixes files, stage and commit them. Covers govet and typecheck — never run `go vet` separately.
- `make lint-check` — linter without auto-fix; the CI zero-issue gate.
- `make test` — run unit tests. Run after lint is clean.
- `make cover` — cross-package coverage with the CI floor (`COVER_MIN`).
- `make deadcode` — fail on functions unreachable from the binaries (runs in CI's lint job). Run after removing a subcommand or feature.
- `make test-e2e` — Go e2e suites in `test/e2e` (build tag `e2e`, always `-count=1`): golden diffs vs committed mrp outputs. **CI only**: locally run the targeted suites a change touches (`go test -tags e2e -count=1 -run '<suite>/<fixture>' ./test/e2e/`, under about 4 minutes); PR Validation runs the full suite (see the `workflow` skill's Verification).
- `make test-mrp-diff` — the differential against a real `mrp` (needs `MARTIAN_BIN`) on its own; `make test-e2e` includes it, and CI runs it on every PR. `make bench` — the data-movement regression gate, also CI only.
- `MARTIAN_BIN` must hold `mrp` and `mrjob` built at the commit `go.mod` pins (`go list -m -f '{{.Version}}' github.com/martian-lang/martian`; CI's `setup-martian` builds exactly that). An `mrp` at another commit makes every differential and every golden you record a comparison against the wrong oracle.

Local runs CI cannot replace: the scale tier (`make test-scale`, weekly nightly only), recording a bench baseline (`BENCH_UPDATE=1`), and the real `mrp` runs that establish a new golden or fixture's ground truth.

## Data Movement: the Efficiency Bar

Tasks are 1:1 with `mrp`'s jobs (the ABI diff counts every stage phase's invocations against `mrp`'s), and each stage task stages only the upstream outs its `_args` read, as an `mrp` job reads only those forks' outs. Superlinear staging is a bug, never a tradeoff. Three layers hold it:

- **`TestStandInStagesOnlyWhatItReads`** (`internal/cg/staging_test.go`) runs every fixture's literal and witness invocation through a stand-in that gives each instance exactly what the driver's `READ`/`GATHER` give it, and fails when an instance is given more instances of a node than its resolution reads. Every new fixture is covered by adding it to `testdata/`. Keep the stand-in (`run_test.go` `readAt`) in step with the driver's read wiring (`internal/cgnf/node.go` `read`): it is the model the guard trusts.
- **Bench lanes with `flatStaging`** hold the most bytes one stage task stages to the same at two widths. A new data-movement shape gets a `bench/<shape>` with two width invocations, real `mrp` runs for `mrpJobs`, an `mrpDiffCases` entry, and baseline entries recorded with `BENCH_UPDATE=1 make bench` — commit only the new lanes' entries, since other lanes' numbers drift with the environment. Prove the gate red on the unfixed code before relying on it.
- **The scale tier** (`test/e2e/scale_test.go`) runs the bench shapes at production widths; a new shape needs a `TestScale<Shape>`, the nightly matrix entry and `SCALE_TIMEOUT` room.

The staged-bytes probe (`bench_metrics.py`) leaves out `call.b64`, the run's invocation: every `mrp` job's `_jobinfo` carries the same invocation with all its args, so its growth with entry values is ABI parity, not orchestration staging.

The correctness bar these suites enforce (see CLAUDE.md "North star"): pipeline
OUTPUTS identical to mrp, plus the stage `_args`/`_outs` ABI. Do NOT write tests
that pin the intermediate task graph or intermediate bundle bytes as a contract
— orchestration between stages is free to change to idiomatic Nextflow, and a
test frozen on today's task shape blocks exactly the overhead-reduction work
the project wants. Pin outputs, exit codes, ordering determinism, and scaling
properties (task/work counts flat in fork width) instead.

Exception: when fixing a bug via the `tdd` skill, run the single reproducing test (`go test -run <TestName> ./<pkg>/`) during the red-green cycle; the full lint+test pass still happens once at the end.

## See Also

- `/go-standards` — full code quality rules
