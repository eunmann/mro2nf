---
description: Create a detailed GitHub Issue by deeply researching the entire codebase, challenging the user's assumptions, and filing the issue with full context. Use when you want to file a well-researched issue for a feature, bug, or improvement.
---

# Create GitHub Issue

> **Mindset (CLAUDE.md Operating Principles):** Be skeptical, critical, and curious. Challenge assumptions — the user's, the repo's, your own — with first principles and evidence from code. No guesses: verify by reading. Do not stop at the first match. Present options with tradeoffs; never claims about size, difficulty, or effort. Pre-production: prefer clean replacement over compatible patching.

Your job is to deeply understand what the user wants, challenge their assumptions with evidence from the codebase, ensure the design is sound, and only then file a well-structured GitHub Issue.

You are a collaborator, not an order-taker. Push back when the user's framing misses important context. Ask hard questions. Surface trade-offs. The goal is an issue that the implementer can trust.

## User's Request

$ARGUMENTS

## Current State

- **Branch:** !`git branch --show-current`
- **Recent commits:** !`git log --oneline -10`

## Step 1: Understand Intent Before Searching

Before touching the codebase, make sure you understand what the user actually wants — not just what they said.

Ask yourself:
- What problem is this solving? Is there a simpler framing?
- What assumptions is the user making about how the system works?
- Are there ambiguous terms that could mean different things in different parts of the codebase?

If anything is unclear, ask the user to clarify before proceeding. Do not guess.

## Step 2: Deep Research

Apply the `repo-research` skill scoped to this request, steps 1–5; the output of `/create-issue` is an issue body, not code.

A good issue captures the change's blast radius so the implementer isn't surprised: every layer (frontend/IR, emitter, `lib/Mro2nf.groovy`, bindspecs, `mre`, shim), target (local, awsbatch, healthomics), fixture, golden, CI suite and doc it needs, each grounded in `file:line`. An `mrp`-parity bug carries a minimal `.mro` repro with the real `mrp` output beside mro2nf's.

## Step 3: Challenge the User's Design

This is the most important step. Before writing the issue, present your findings to the user and challenge their assumptions. Use AskUserQuestion or direct conversation to drive alignment.

### What to challenge

**Does the request match reality?**
- "You asked for X, but the codebase already does Y. Did you mean to extend Y, or replace it?"
- "This feature assumes Z exists, but I found that Z was removed in commit abc123."

**Are there simpler alternatives?**
- "You described building a new service for this, but the existing FooService already handles 80% of the use case. Would extending it be better?"
- "This could be done in the emitter alone — `mre` already supports it. Want to go that route?"

**What are the trade-offs they haven't considered?**
- "This approach requires a new table, but it means N+1 queries in the hot path. The alternative is denormalizing into the existing table."
- "Adding this field to the API response is a breaking change for existing clients."

**Is the scope right?**
- "These steps are independently mergeable and touch unrelated subsystems — should they be separate issues?"
- "This is listed as a bug fix, but the behavior is actually by design — see the comment in handler.go:42. Is this a feature request instead?"
- "The clean fix is to replace this format/API outright rather than support both — pre-production, no compatibility shim needed."

**What's missing from their thinking?**
- "You didn't mention tests, but this changes generated Nextflow, so the e2e goldens and the lint-nf matrix cover it."
- "This changes process names, so every `-resume` cache is invalidated once."

### How to challenge

- Be direct and specific. Reference file paths, function names, and line numbers.
- Present evidence, not opinions. "The code does X" beats "I think X might be an issue."
- Offer alternatives with clear trade-offs. Don't just say "this is wrong" — say "here's a better approach and why."
- Ask for decisions when there are genuine forks: "Do you want A (simpler but limited) or B (more complex but extensible)?"

Wait for the user to respond before proceeding. Iterate if needed — go back to `repo-research` if the conversation reveals new areas to explore.

## Step 4: Write the Solution Plan

Once aligned with the user, write a concrete, ordered implementation plan:

- Break the work into sequential steps that each result in a working state
- For each step, name the specific files that change and describe the modification
- Call out new files, packages, or fixtures that need to be created — and old code to delete (no compatibility shims)
- Specify required tests — unit, and the e2e/mrp-diff fixtures with their real-`mrp` goldens
- Flag decisions that the implementer needs to make

Do not estimate complexity, effort, or time — describe what the code requires, nothing more.

Present the plan as a numbered list. Each item must name the file path, the change, and the rationale. Then write exactly:

> Approve this plan? Reply "approved" to proceed, or describe changes to adjust.

Do not proceed to filing until the user responds with explicit approval. Interpret ambiguous replies as "not yet" and ask a clarifying question.

## Step 5: Labels

Every issue gets exactly one **type** label, one **priority** label, the **area** labels it touches, and a **milestone**. Never add size labels. `gh label list` and `gh api repos/{owner}/{repo}/milestones` show the current sets.

### Type (required — pick exactly one)

| Label | When to use |
|-------|-------------|
| `bug` | Something is broken or produces wrong results |
| `enhancement` | New feature or improvement to existing behavior |
| `documentation` | Docs-only change (no code) |
| `github_actions` | CI workflow change |

### Area (pick all that apply)

`area:frontend`, `area:bind`, `area:shim`, `area:emit`, `area:overrides`, `area:runner`, `area:config`, `area:ci`, `area:deploy`, `area:testing`

### Priority (required)

| Label | When to use |
|-------|-------------|
| `P0-wrong-output` | Outputs or a stage's `_args`/`_outs` silently differ from `mrp` in the default config |
| `P1` | A silent no-op or a success where `mrp` fails, a broken feature, opt-in data corruption, security, or a test gap that lets a P0 through |
| `P2` | Robustness, a loud failure, or an opt-in-only problem |
| `P3` | Low impact or hygiene |

### Special (add when applicable)

`security`, `simplification` (a step of the simplification epic), `needs-verification` (plausible, not yet reproduced), `epic` (a parent issue), `batch` (the issues one PR lands together)

### Milestone (required)

Pick the roadmap milestone the work belongs to (M0 Oracle → M5 Packaging + docs + hygiene); the master index issue lists their goals and exit criteria.

## Step 6: Place the Issue in the Tree

Relationships carry all structure, never body text. GitHub shows sub-issues, progress and `blocked by` links on every issue, and keeps them current as issues close; a list of issue numbers in a body goes stale the moment one is filed or closed.

The tree is: master index (#283) → epics (label `epic`) → batches (label `batch`) → issues. A **batch** is the set of issues one PR lands together, because they share files and code paths. Its PR closes every sub-issue.

1. **Find the parent.** Read the open batches (`gh issue list --label batch`) and their sub-issues.
   - If one shares files or code paths with the new issue, file the issue under that batch.
   - Otherwise, file it under the epic that owns the area, and create a batch when two or more of the epic's issues would share a PR.
   - An issue has exactly one parent.
2. **Add dependencies.** When the work can't start until another issue lands, add `blocked by` links. Link issue to issue for a precise dependency, and batch to batch for the order of whole PRs.
3. **Split work across issues, not checklists.** Work that spans several PRs becomes a parent with one sub-issue per PR. An epic's remaining work must exist as open sub-issues, never as checklist lines in its body.

Don't write `Part of #N`, related-issue lists or dependency lists in the body. Don't edit the master index either: it never lists issues. Mention another issue in prose only where the reasoning needs it, for example "mrp gives X here, as in #253".

Set relationships with `gh` (v2.94+):

```bash
gh issue create ... --parent <batch-or-epic> --blocked-by <n>,<m>   # at creation
gh issue edit <n> --parent <new-parent>                             # move: replaces the old parent
gh issue edit <n> --add-blocked-by <m>                              # add a dependency later
```

An issue split out later, during implementation, review or user testing, is placed the same way when it is filed.

## Step 7: Create the Issue

Create the GitHub Issue using `gh issue create`. Always include the `--label`, `--milestone` and `--parent` flags, plus `--blocked-by` when Step 6 found a dependency.

```
gh issue create \
  --title "<concise title, under 70 chars>" \
  --label "<type>" --label "<area>" --label "<priority>" \
  --milestone "<milestone title>" \
  --parent <batch or epic number> \
  --blocked-by <issue numbers, when any> \
  --body "$(cat <<'EOF'
## Summary

<1-3 sentences: what this is and why it matters>

## What to Remove

<Anything that should be deleted, if applicable. Include file paths.>

## What to Build

<Detailed description of what needs to be built, organized by component.>

## Implementation Plan

### Step 1: <Step name>
- Files: `path/to/file.go`
- <What changes and why>

### Step 2: <Step name>
- Files: `path/to/file.go`, `path/to/other.go`
- <What changes and why>

<Continue for all steps...>

## Key Files

| Area | Path |
|------|------|
| Core | `internal/...` |
| Runtime | `cmd/mre/...` |
| Tests | `internal/.../..._test.go` |
| Fixtures | `testdata/<name>/` |

## Testing

- [ ] <Specific test: what it covers and which file>
- [ ] <Another test>
- [ ] e2e fixture: <what scenario, golden recorded from real `mrp`>

## Verification

- [ ] Locally: `make lint`, `make test`, and targeted e2e for <the fixtures and suites this touches>
- [ ] PR Validation is green (full e2e with the mrp differential, docker, lint-nf, bench, cover)

## Risks

- <Any risks, unknowns, or decisions needing input>

EOF
)"
```

After creating the issue, display the issue URL, the labels applied, its parent and blocked-by links, and a brief summary of what was filed.
