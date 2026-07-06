---
name: workflow
description: Branch creation, incremental change process, and PR steps. Use when starting new work, planning a feature, or preparing to submit a PR.
---

# Workflow

> **Mindset (CLAUDE.md Operating Principles):** Challenge assumptions — the user's, the repo's, your own — with evidence from code. No guesses: verify by reading. Sprawl into related features before changing anything. Present options with tradeoffs; never commentary on size or difficulty. No hacks, no workarounds, no backwards-compatibility shims. Refactor when the code warrants it.

## Current State

- **Branch:** !`git branch --show-current`
- **Status:** !`git status --short`
- **Recent commits:** !`git log --oneline -5`

## Branch Creation

Before branching, challenge whether this is the right scope — a single branch may need splitting or folding into existing work. Cite the evidence: what does the code actually require? Do not frame scope as "big" or "small" — frame it as what is independently mergeable.

- **New feature:** `git checkout main && git pull origin main && git checkout -b feature/<short-name>`
- **Extending a feature:** `git checkout feature/<parent-branch> && git pull origin feature/<parent-branch> && git checkout -b feature/<short-name>`

## Worktrees for Stacked / Parallel Work

When landing several independent or stacked changes (e.g. a batch of issues), use a
git worktree per branch so the primary checkout stays clean and branches don't clobber
each other's working tree:

- **Independent PR off main:** `git worktree add ../wt-<short-name> -b feature/<short-name> main`
- **Stacked PR** (builds on an unmerged parent): branch off the parent instead of main:
  `git worktree add ../wt-<short-name> -b feature/<short-name> feature/<parent-branch>`
- Work inside the worktree dir; run `make lint`/`make test` there.
- After the branch merges, remove it: `git worktree remove ../wt-<short-name>` and
  `git branch -d feature/<short-name>`.
- **Stacking rule:** put issues that touch the same files on the *same* stack (parent→child)
  so they merge in order without conflicts; put issues on disjoint file sets on separate
  worktrees branched off main so they can proceed in parallel. Squash-merge a stack by
  collapsing tree-based (`reset --soft`), never `merge -X ours` (corrupts history).

## Implementation

1. **Sprawl first.** Read the related features, consumers, and call paths the change touches. A transpiler change usually spans layers — check whether the frontend/IR, emitter, shim, and bindspec sides all agree before editing one of them.
2. **Implement directly.** Write code and its tests together. Use the `tdd` skill only for bug fixes, where the failing test proves the bug.
3. **Refactor where warranted.** If existing code near the change should be restructured for clarity or correctness, do it — in its own commit. No hacks, no workarounds, no compatibility shims (pre-production).
4. **Edit discipline.** Edit/Write tools for all source edits. Complete the full edit set before any lint or test run.

### Atomic Commits

- One logical change per commit: a feature + its tests, a refactor, or a bug fix.
- Each commit compiles and passes tests independently.
- Messages describe behavior, not files: `fix: zero-chunk split starves its JOIN` not `update generate.go`.

## Verification (end of change set, not mid-development)

1. `make lint` — commit any auto-fixes.
2. `make test` — all green. Before the PR: `make lint-check`, `make cover`, and
   `make test-e2e` (plus `make test-e2e-docker` when the emitter, shim, or data
   plane changed).

## Review

**Required before every merge:** run `/code-review max --fix` on the branch diff. This
runs the deep multi-agent review and applies the surviving findings to the working tree.
After it applies fixes, re-run `make lint` + `make test`, then commit the fixes
(`fix: address code-review findings`). Do not open/merge a PR that skipped this step — it
is part of the definition of done, not an optional polish pass. Plain `/code-review` (no
`max`) is only for a quick local sanity check mid-development.

## Finishing Up

1. Summarize what changed and why — no commentary on effort or scale.
2. Push the branch and open a PR with `gh pr create`; base a stacked PR on its parent
   branch (`gh pr create --base feature/<parent-branch>`), not `main`.
3. Enable auto-merge (`gh pr merge --auto --squash`) and let the PR Validation workflow
   (lint, build, unit tests, both e2e suites, bench) go green before it merges. Never push
   directly to `main`. For a stack, the parent must merge before the child (retarget the
   child to `main` once the parent lands, or merge in order).
4. After merge, remove the worktree and delete the local branch:
   `git worktree remove ../wt-<short-name>` and `git branch -d feature/<short-name>`.
