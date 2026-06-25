---
name: tdd
description: "Failing-test-first cycle for bug fixes and regressions ONLY. Do not use for new features — implement those with tests written alongside in the same commit."
---

# TDD — Bug Fixes and Regressions Only

> **Scope rule:** Failing-test-first applies only when there is a bug to prove. For new features, write implementation and tests together in one commit — do not deliberately write failing tests for behavior that does not exist yet.

> **Mindset (CLAUDE.md Operating Principles):** Challenge assumptions with evidence from code. No guesses: verify by reading. Sprawl into related code before changing anything.

## Current State

- **Branch:** !`git branch --show-current`
- **Status:** !`git status --short`

## Cycle

### Red — Reproduce the Bug with a Failing Test

Before writing the test, verify you understand the actual broken behavior — trace the call path and read the code. A test encoding a misread of the bug passes for the wrong reasons.

1. Write the smallest test that reproduces the bug. Table-driven with `t.Run`, standard `testing` package, `cmp.Diff` for structs.
2. Run the single test (`go test -run <TestName> ./<pkg>/`). Confirm it **fails for the expected reason** — the failure is the proof of the bug.

### Green — Fix It

1. Fix the root cause. No hacks, no workarounds — if the proper fix requires restructuring, restructure.
2. Run the single test again. It passes.

### Finish — Verify Once, Commit

1. If the fix exposed code that should be refactored, refactor it now (separate commit if it is a distinct logical change).
2. `make lint` — stage any auto-fixes. `make test` — all green. Run these once at the end, not between micro-steps.
3. Commit: test + fix together, message describing the bug and the behavior now guaranteed.

## Repeat

One bug per cycle. The regression test stays forever.
