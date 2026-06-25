---
name: go-standards
description: "Reference: Go code quality standards — control flow, error handling, functions, concurrency, naming, architecture. Consult when writing or reviewing Go code, not as an action to run."
---

# Go Standards

## Read First, Write Second

- Read neighboring code before writing. Match the existing style, error handling, naming, and structure — but challenge it: existing patterns are claims, not proof of correctness. If the surrounding pattern is wrong, fix the pattern, don't copy it.
- Evaluate whether existing code near your changes should be refactored for clarity, maintainability, or correctness. If it should, refactor it in its own commit.
- Run `make help` to see all available targets.

## Pre-Production Stance

- Backwards compatibility does not matter yet. No compat shims, no deprecated-but-kept symbols, no dual code paths supporting old and new formats. Replace cleanly and delete the old code in the same change.
- Never hack or work around. A fix that fights the design is wrong even if it passes tests — change the design.
- Delete dead code on sight: unused functions, fields, flags, and files. Unreferenced code is a maintenance tax with no payer.

## Control Flow

- Use simple, explicit control flow. No `goto`. No recursion unless a provable depth bound is documented.
- Prefer early returns for errors to keep the happy path flat and un-indented.
- Centralize branching in parent functions. Helpers compute and return values — they do not make high-level control decisions.

## Bounds and Allocation

- Every loop over dynamic data must have a fixed upper bound. Unbounded retries, unbounded channel buffers, and open-ended polling are prohibited.
- Every retry must have a max attempt count and bounded backoff. Use `context.Context` for cancellation.
- Pre-allocate slices and maps with known capacity: `make([]T, 0, n)`, `make(map[K]V, n)`. Avoid `append` that triggers reallocation in hot paths.

## Functions

- Maximum 70 lines per function. One job per function — if describing it requires "and", split it.
- Validate all inputs at the top of the function. Return errors for bad input; `panic` only for invariant violations that indicate programmer bugs.
- If a function takes more than 3-4 parameters of the same type, use an options struct to prevent argument swaps.
- Keep helpers pure (no side effects) when possible.

## Error Handling

- Check every returned error. Never discard an error silently.
- Wrap errors with context: `fmt.Errorf("operation: %w", err)`. Do not use string matching to inspect errors — use `errors.Is` and `errors.As`.
- Define sentinel errors as package-level vars: `var ErrNotFound = errors.New("not found")`.
- Do not use `panic` for normal error handling. `panic` in a library is a bug.
- If a return value is intentionally unused, assign to `_` with a comment explaining why.

## Types and Interfaces

- Accept interfaces, return structs. Define interfaces at the consumer, not the producer.
- Do not define an interface alongside its only implementation — that is premature abstraction.
- Keep interfaces small and focused: 1-3 methods. The ideal Go interface is one method.
- Return concrete types from constructors so new methods can be added without breaking consumers.
- Prefer functions over methods when no state is needed. Do not create a struct to hold a single method.

## Scope and State

- Declare variables at the smallest possible scope. Use `:=` and `if` initializers to confine variables.
- Do not use package-level mutable variables. Inject dependencies through struct fields or function parameters.
- Do not reuse a variable for multiple unrelated purposes.
- Group a mutex and the state it protects into a struct. Do not expose the mutex — provide methods that lock internally.

## Concurrency

- Every goroutine must have a clear owner and a clear shutdown path.
- Never fire-and-forget goroutines. Use `sync.WaitGroup` or `errgroup.Group` to wait for completion.
- Pass `context.Context` as the first parameter. Never store it in a struct field.
- Every channel must have a declared buffer size, or the goroutine lifecycle must guarantee no blocking.
- If you can't diagram the concurrency on a napkin, simplify it.

## Database & SQL

Apply whenever you write, edit, or call SQL. Schema and indexes live in `infra/migrations/` (gamedev DB: `infra/migrations-gamedev/`).

- **Index what you filter.** Every column used in a `WHERE`, `JOIN`, or `ORDER BY` on a growing table needs an index (composite in predicate order, with the `ORDER BY` column trailing). Foreign-key columns need their own index — unindexed FKs cause seq scans and lock contention on parent mutations. A column that is only the *non-leading* member of a composite is not covered for a standalone lookup.
- **No redundant indexes.** A single-column index that is a leading prefix of an existing composite (or unique) index is dead write-amplification — do not add it, and drop it if you find one.
- **Keep predicates sargable.** No function/cast on the indexed column in the `WHERE` (`LOWER(col)=`, `id::text LIKE`); use an expression index or a trigram GIN (`gin_trgm_ops`) for substring/`ILIKE '%x%'` search. A leading-wildcard `LIKE` without a trigram index is a seq scan.
- **Avoid N+1.** Never call a per-row repo method inside a loop over a result set. Add a batch method (`WHERE id = ANY($1)` returning a `map[ID]T`) and resolve from the map. If a record already carries the foreign id (e.g. `match_participants.user_id`), use it instead of a lookup.
- **Paginate growing reads.** No unbounded `SELECT` over a growing table; use `LIMIT`, and prefer keyset pagination over deep `OFFSET`. Split projections so list paths do not fetch large `TEXT`/`BYTEA`/`TOAST` columns that only a detail view renders.
- **Parameterize and scan exactly.** All values via `$N` placeholders — never string-interpolate. Scan destinations must match the `SELECT`/`RETURNING` columns in count and order; nullable columns map to `*T`/`sql.NullT`. Multi-statement writes go in one transaction (or a CTE).
- **Verify the plan.** When you add or change a query or index, prove it with `EXPLAIN` against the live DB. Dev tables are tiny, so the planner seq-scans them harmlessly — run `SET enable_seqscan=off; EXPLAIN <query>` (add `enable_indexscan/indexonlyscan=off` to force a GIN bitmap) and confirm the access path uses the intended index. A `Seq Scan` on a growing table with no index fallback is a defect, not a dev-data artifact. These EXPLAIN runs are throwaway — do not commit harness scripts or audit reports.

## Pointers and Unsafe

- Do not use `unsafe` unless absolutely necessary, isolated, and commented with justification.
- Do not use `reflect` in hot paths.
- Avoid pointer-to-pointer indirection. If a function accesses its argument only as `*x`, pass the value directly.
- Use pointer receivers when the method mutates the receiver or the struct is large; value receivers for small immutable types. Do not mix receiver types on a single struct.

## Naming

- `MixedCaps` (exported), `mixedCaps` (unexported). Never `snake_case` for Go identifiers.
- Short receiver names: one or two letters abbreviating the type (`c` for `Client`). No `self`, `this`, or `me`.
- No stutter: `user.New`, not `user.NewUser`. The package name is context.
- Standard initialisms in all-caps: `ID`, `URL`, `HTTP`, `API`.
- Add units to bare numeric names: `timeoutMs`, `sizeBytes`, `retryDelaySeconds`.
- Package names describe what the package provides, not what it contains. Never `util`, `common`, `helpers`, or `misc`.

## Documentation

- Doc comments on every exported symbol. Start with the symbol name: `// ParseAge parses...`.
- Comments explain *why*, not *what*. The code shows what; the comment explains the rationale, trade-off, or non-obvious invariant.
- Do not add a comment that restates what the code or config line already says. No narrating an edit, no justifying an obvious choice. If it carries no information beyond the line below it, delete it. This applies to config files too, not just Go.
- Comments are sentences: capital letter, full stop.

## Architecture

- Every package has a single, clear purpose. Dependency graph must be a DAG — no import cycles.
- Keep `main` thin: parse config, wire dependencies, call `Run`.
- Justify every external dependency. Prefer the standard library. Do not vendor a library for a single utility function.
- Pin dependencies to exact versions. Review dependency updates before merging.

## See Also

- `/testing` — test conventions and lint rules
