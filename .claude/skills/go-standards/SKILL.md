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

### A comment is usually a symptom

The bar for adding one is that the code *cannot* be made to say it. Most lines
clear that bar by being rewritten, not annotated. Before writing a comment, try
these in order:

1. **Rename.** A comment defining what a variable, field, or function is, is a
   naming bug. Fix the name.
2. **Extract.** A comment introducing a block — `// now validate the inputs` —
   is a function waiting to be named.
3. **Restructure.** A comment warning a caller about call order, nil-ness, or a
   field that must not be set describes a type or signature that permits the
   mistake. Make it impossible instead.

Only when all three fail is a comment the right tool, and that is precisely when
the reason lives *outside* the code: a Martian runtime behaviour, a Nextflow
semantic, a wire format, a bug the shape of the code exists to prevent, a
rejected alternative that looks better than it is. Those cannot be renamed into
existence.

"This code needs a comment to be followed" is first a report about the code.

### Delete on sight

Dead comments go the way of dead code, whether or not this change introduced
them. If you are editing a file and pass a comment that restates its line,
narrates an old edit, or describes behaviour that is no longer true, delete it
in that commit — do not leave it for a cleanup that never comes.

Remove it outright when it adds nothing. Do not reword a worthless comment into
a better-written worthless comment. A stale comment is worse than none, because
it is believed.

### Length

A comment is as long as its reason and no longer. One or two lines carries
almost every *why*. Past about six lines, ask what a reader loses if you cut it
in half — usually nothing, because the extra lines re-explain the first ones.

Earn the length only for a reason the reader genuinely cannot reconstruct: an
external constraint (`mrp`'s behaviour, a Nextflow semantic, a wire format), a
bug the shape of the code exists to prevent, or a rejected alternative that
looks obviously better than it is. Never for a summary of what the function
does.

### The test to apply

**Would this comment exist if the code had been written this way the first
time?** If it only makes sense as a record of how the code got here, cut it.
These all fail that test:

- `// The previous implementation used X, which broke because…`
- `// Round two found that…`, `// This was added to fix…`, `// (#123)`
- Restating the diff: `// Now derives the value from the constant`
- A doc comment whose first sentence is the function signature in prose

Keep the *fact* the history taught, drop the history:

```go
// BAD — narrates how we got here
// This used to gather with .collect(), whose completion order leaked into
// the JOIN's -resume key, so we switched to toSortedList in #87.

// GOOD — states the constraint
// Sorted by name: completion order would otherwise enter the JOIN's
// -resume cache key, and toSortedList emits [] for a zero-chunk split.
```

## Architecture

- Every package has a single, clear purpose. Dependency graph must be a DAG — no import cycles.
- Keep `main` thin: parse config, wire dependencies, call `Run`.
- Justify every external dependency. Prefer the standard library. Do not vendor a library for a single utility function.
- Pin dependencies to exact versions. Review dependency updates before merging.

## See Also

- `/testing` — test conventions and lint rules
