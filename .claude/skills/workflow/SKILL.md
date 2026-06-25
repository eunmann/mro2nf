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

1. **Sprawl first.** Read the related features, consumers, and call paths the change touches. For non-trivial work, apply the `repo-research` skill. If the work touches handlers, templates, or JS, apply the `frontend` skill's mandatory pre-work checklist.
2. **Implement directly.** Write code and its tests together. Use the `tdd` skill only for bug fixes, where the failing test proves the bug.
3. **Refactor where warranted.** If existing code near the change should be restructured for clarity or correctness, do it — in its own commit. No hacks, no workarounds, no compatibility shims (pre-production).
4. **Edit discipline.** Edit/Write tools for all source edits. Complete the full edit set before any lint or test run.

### Atomic Commits

- One logical change per commit: a feature + its tests, a refactor, or a bug fix.
- Each commit compiles and passes tests independently.
- Messages describe behavior, not files: `fix: movement cancels channels` not `update movement.go`.

## Verification (end of change set, not mid-development)

1. `make lint` — commit any auto-fixes.
2. `make test` — all green. `make test-all` before the PR.

## Review

Run `/review-loop` to find and fix bugs, gaps, and quality issues iteratively until clean.

## Finishing Up

1. Summarize what changed and why — no commentary on effort or scale.
2. Ask: "Ready to create a draft PR?" (`/create-pr`)
3. After merge, delete the local branch: `git branch -d feature/<short-name>`
