---
description: Quick pre-commit sanity check for items the linter cannot catch. Run before finishing a change.
---

# Pre-Commit Checklist

The linter catches formatting, function length, magic numbers, doc comments, and method ordering. This checklist covers what it cannot:

```
[ ] Every error is wrapped with context: fmt.Errorf("operation: %w", err)
[ ] No errors silently discarded (assign to _ only with a comment explaining why)
[ ] Tests cover both valid and invalid inputs, plus edge cases (nil, zero, empty, boundaries)
[ ] No global mutable state — dependencies injected via struct fields or parameters
[ ] Interfaces defined at the consumer, not the producer. Return concrete types.
[ ] context.Context is the first parameter for anything with I/O or cancellation
[ ] New code follows existing patterns in the package (read neighboring code first)
[ ] No hacks, workarounds, or backwards-compatibility shims — old code replaced is old code deleted
[ ] No comments that narrate the edit or restate the code — only non-obvious why
[ ] No comment would read as a changelog to someone who never saw the diff
[ ] Every comment over ~6 lines earns it; the rest say their reason once
[ ] Comments you passed while editing — old ones included — are deleted, not reworded
[ ] Nothing needed a comment to be followed; where it did, the code was renamed or split instead
[ ] No TODOs (unless explicitly approved)
[ ] Both sides of every seam the change crosses agree: internal/cgnf ↔ mre flags and records, internal/cg records ↔ assets/stage.nf Groovy helper ordering
[ ] Generated Nextflow interpolates nothing unescaped: every baked string goes through the quoting helpers
[ ] Nothing new scales with fork width or chunk count: no per-fork task, no per-instance copy of a whole collection
```

With all edits complete, run `make lint`, then `make test`. Both clean before committing.
