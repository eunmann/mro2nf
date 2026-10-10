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

## Implementation

1. **Sprawl first.** Read the related features, consumers, and call paths the change touches. For non-trivial work, apply the `repo-research` skill. A transpiler change usually spans layers — check whether the frontend/IR, emitter, shim, and bindspec sides all agree before editing one of them.
2. **Implement directly.** Write code and its tests together. Use the `tdd` skill only for bug fixes, where the failing test proves the bug.
3. **Refactor where warranted.** If existing code near the change should be restructured for clarity or correctness, do it — in its own commit. No hacks, no workarounds, no compatibility shims (pre-production).
4. **Edit discipline.** Edit/Write tools for all source edits. Complete the full edit set before any lint or test run.

### Atomic Commits

- One logical change per commit: a feature + its tests, a refactor, or a bug fix.
- Each commit compiles and passes tests independently.

### Commit Messages

Every commit in the repo, whoever makes it, uses [Conventional Commits](https://www.conventionalcommits.org), plus a `Task:` label saying which job produced it. PR titles use the same `type(scope): summary` header, because a squash merge makes the PR title the commit on `main`.

```
fix(bind): narrow a struct value to the receiving struct's fields

Why the change was needed and what it does, in a few lines.

Task: impl #239
Co-Authored-By: ...
```

- **Header:** `type(scope): summary`, at most 72 characters. The summary is imperative, lowercase and has no trailing period. It describes behaviour, not files: `fix(emit): zero-chunk split no longer starves its join`, not `update generate.go`.
- **Type:** `feat` new behaviour · `fix` bug fix · `refactor` no behaviour change · `perf` · `test` tests only · `docs` · `build` Makefile, deps, generators · `ci` · `chore` everything else.
- **Scope:** the area touched, as one short word: a package or layer (`frontend`, `bind`, `shim`, `emit`, `mre`, `overrides`, `runner`, `config`, `e2e`, `bench`, `deploy`). Omit the scope when the change spans many areas.
- **Body:** optional. Keep it to the why and anything a reviewer can't see in the diff.
- **`Task:` label** (a footer, before any trailers the session adds):
  - `impl #<issue>`: implementing an issue. Use `impl #238 #246` when one commit serves several.
  - `review R<round>`: a review-loop fix, e.g. `review R2`.
  - `pr #<number>`: addressing PR review comments.
  - `manual`: anything else.

## Verification (end of change set, not mid-development)

1. `make lint` — commit any auto-fixes.
2. `make test` — all green. Before the PR: `make lint-check`, `make cover`, and `make test-e2e` (plus `make test-e2e-docker` when the emitter, shim, or data plane changed, `make lint-nf` when generated Nextflow changed, `make bench` when task counts or bytes staged can change). `make test-e2e` includes the mrp differential when `MARTIAN_BIN` holds an `mrp` build; without one it skips locally, and CI runs it.

## Review

Run `/review-loop`: one full review of the branch, then checks of its own fixes, at most three rounds.

## Finishing Up

1. Summarize what changed and why — no commentary on effort or scale.
2. Open the PR (`/create-pr`).
3. **Before merging**, in this order:
   1. `/review-loop` has finished on the branch.
   2. Copilot has reviewed the PR's current head. Run `.claude/scripts/wait-copilot-review.sh <pr>` in the background (it requests the review if needed and notifies once it lands; reviews take a few minutes); don't poll by hand.
   3. Every Copilot thread is addressed with `/address-pr-comments`.
   4. PR Validation (lint, build, unit tests, both e2e suites, lint-nf, bench) is green on the final head. Branch protection requires the branch to be up to date with `main`, so update it after another PR lands.
   5. Merge with `gh pr merge --squash --delete-branch`, or enable auto-merge (`gh pr merge --auto --squash`) once steps 1–3 are done. Never push directly to `main`.
4. After merge, remove the worktree and delete the local branch: `git worktree remove ../wt-<short-name>` and `git branch -d feature/<short-name>`.

## Working Multiple Issues / PRs at Once

- Give every branch its own git worktree; do all edits, builds, and tests for that branch inside its worktree. Never interleave branch checkouts in the main tree while other work (or a subagent) is reading it.
  - **Independent PR off main:** `git worktree add ../wt-<short-name> -b feature/<short-name> main`
  - **Stacked PR** (builds on an unmerged parent): `git worktree add ../wt-<short-name> -b feature/<short-name> feature/<parent-branch>`, and `gh pr create --base feature/<parent-branch>`.
- **Stacking rule:** put issues that touch the same files on the *same* stack (parent→child) so they merge in order without conflicts; put issues on disjoint file sets on separate worktrees branched off main so they can proceed in parallel. For a stack, the parent merges before the child; retarget the child to `main` once the parent lands.
- Delegate per-branch units of work (implementation, review passes, fixes) to subagents scoped to one worktree each, and say which worktree in the prompt.
- Throttle host load: run one heavy phase at a time — one branch's e2e suite or one review fan-out, not several concurrently. Each `nextflow run` is a JVM.
- `make lint` in a worktree runs as `GOLANGCI_LINT_CACHE=$PWD/.golangci-cache TMPDIR=$PWD/.tmp make lint` (create `.tmp` first; remove both afterwards). The shared lint cache keys entries by path, so a worktree run on it can apply stale-offset fixes to another checkout's files and leave entries the main checkout later reports as issues; a per-worktree cache and lock avoid both.
