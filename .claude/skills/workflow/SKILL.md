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

1. **Sprawl first.** Read the related features, consumers, and call paths the change touches. For non-trivial work, apply the `repo-research` skill. A transpiler change usually spans layers — check whether the frontend/IR, call graph, emitter, job runner, shim and `mre` sides all agree before editing one of them.
2. **Implement directly.** Write code and its tests together. Use the `tdd` skill only for bug fixes, where the failing test proves the bug.
3. **Refactor where warranted.** If existing code near the change should be restructured for clarity or correctness, do it — in its own commit. No hacks, no workarounds, no compatibility shims (pre-production).
4. **Edit discipline.** Edit/Write tools for all source edits. Complete the full edit set before any lint or test run.

### Atomic Commits

- One logical change per commit: a feature + its tests, a refactor, or a bug fix.
- Each commit compiles and passes tests independently.

### Commit Messages

Every commit in the repo, whoever makes it, uses [Conventional Commits](https://www.conventionalcommits.org), plus a `Task:` label saying which job produced it. PR titles use the same `type(scope): summary` header, because a squash merge makes the PR title the commit on `main`.

```
fix(cg): narrow a struct value to the receiving struct's fields

Why the change was needed and what it does, in a few lines.

Task: impl #239
Co-Authored-By: ...
```

- **Header:** `type(scope): summary`, at most 72 characters. The summary is imperative, lowercase and has no trailing period. It describes behaviour, not files: `fix(cgnf): zero-chunk split no longer starves its join`, not `update generate.go`.
- **Type:** `feat` new behaviour · `fix` bug fix · `refactor` no behaviour change · `perf` · `test` tests only · `docs` · `build` Makefile, deps, generators · `ci` · `chore` everything else.
- **Scope:** the area touched, as one short word: a package or layer (`frontend`, `cg`, `cgnf`, `job`, `shim`, `mre`, `config`, `e2e`, `bench`, `deploy`). Omit the scope when the change spans many areas.
- **Body:** optional. Keep it to the why and anything a reviewer can't see in the diff.
- **`Task:` label** (a footer, before any trailers the session adds):
  - `impl #<issue>`: implementing an issue. Use `impl #238 #246` when one commit serves several.
  - `review R<round>`: a review-loop fix, e.g. `review R2`.
  - `pr #<number>`: addressing PR review comments.
  - `manual`: anything else.

## Verification (end of change set, not mid-development)

Locally, run only the fast checks; PR Validation is the full gate.

1. `make lint`, and commit any auto-fixes.
2. `make test`, all green.
3. Targeted e2e for what the change touches: `go test -tags e2e -count=1 -run '<the touched fixtures and suites>' ./test/e2e/`. Keep each run under about 4 minutes, because an agent's prompt cache expires after 5 idle minutes.

Do not run the full `make test-e2e`, `make lint-nf`, `make bench` or `make cover` locally before a PR. PR Validation runs all of them, plus `-race`, govulncheck and deadcode, on GitHub's runners. When a run fails, read only the failing job's log tail (`gh run view <id> --log-failed | tail -n 100`) and fix that.

## Review

Run `/review-loop`: one full review of the branch, then checks of its own fixes, at most three rounds.

## Finishing Up

1. Summarize what changed and why — no commentary on effort or scale.
2. Open the PR (`/create-pr`).
3. **Before merging**, in this order:
   1. `/review-loop` has finished on the branch.
   2. Every open review thread (from a person, or from an optional Copilot review) is addressed with `/address-pr-comments`.
   3. PR Validation (lint, build, unit tests, both e2e suites, lint-nf, bench) is green on the final head. Branch protection requires the branch to be up to date with `main`, so update it after another PR lands.
   4. Merge with `gh pr merge --squash --delete-branch`, or enable auto-merge (`gh pr merge --auto --squash`) once steps 1–2 are done. Never push directly to `main`.

   A Copilot review is optional, since `/review-loop` is the required review. When the account has Copilot quota, `.claude/scripts/wait-copilot-review.sh <pr>`, run in the background, requests one and notifies once it lands.
4. After merge, remove the worktree and delete the local branch: `git worktree remove ../wt-<short-name>` and `git branch -d feature/<short-name>`.

## Working Multiple Issues / PRs at Once

- Give every branch its own git worktree; do all edits, builds, and tests for that branch inside its worktree. Never interleave branch checkouts in the main tree while other work (or a subagent) is reading it.
  - **Independent PR off main:** `git worktree add ../wt-<short-name> -b feature/<short-name> main`
  - **Stacked PR** (builds on an unmerged parent): `git worktree add ../wt-<short-name> -b feature/<short-name> feature/<parent-branch>`, and `gh pr create --base feature/<parent-branch>`.
- **Stacking rule:** put issues that touch the same files on the *same* stack (parent→child) so they merge in order without conflicts; put issues on disjoint file sets on separate worktrees branched off main so they can proceed in parallel. For a stack, the parent merges before the child; retarget the child to `main` once the parent lands.
- Delegate per-branch units of work (implementation, review passes, fixes) to subagents scoped to one worktree each, and say which worktree in the prompt.
- Throttle host load: run one heavy phase at a time — one branch's e2e suite or one review fan-out, not several concurrently. Each `nextflow run` is a JVM.
- `make lint` in a worktree runs as `GOLANGCI_LINT_CACHE=$PWD/.golangci-cache TMPDIR=$PWD/.tmp make lint` (create `.tmp` first; remove both afterwards). The shared lint cache keys entries by path, so a worktree run on it can apply stale-offset fixes to another checkout's files and leave entries the main checkout later reports as issues; a per-worktree cache and lock avoid both.
