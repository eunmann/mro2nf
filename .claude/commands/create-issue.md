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

`security`, `simplification` (a step of the simplification epic), `needs-verification` (plausible, not yet reproduced), `epic` (a parent issue)

### Milestone (required)

Pick the roadmap milestone the work belongs to (M0 Oracle → M5 Packaging + docs + hygiene); the master index issue lists their goals and exit criteria.

## Step 6: Scope Check — Single Issue or Parent + Children

Before filing, decide whether this is one issue or a parent with sub-issues:

- **Single issue:** The work can be done in one PR with a clear scope. File one issue.
- **Parent issue with children:** The work spans multiple independent PRs or has distinct phases. File a parent issue first, then create child issues and link them as sub-issues.

**When to create a parent issue:**
- The implementation plan has 3+ steps that are independently mergeable
- Different steps touch unrelated subsystems (e.g., binder + emitter + packaging changes)
- The user explicitly asks for it to be broken up

**Workflow for parent + children:**

1. Create the parent issue first (using the Step 7 template). Its body should describe the overall goal and list the planned sub-issues in a summary section.
2. Create each child issue. Each child's body should reference the parent: `Part of #<parent-number>` at the top.
3. Link each child as a sub-issue of the parent using the GitHub GraphQL API:

```bash
# Get the parent issue's node ID
PARENT_ID=$(gh issue view <parent-number> --json id --jq '.id')

# Get the child issue's node ID
CHILD_ID=$(gh issue view <child-number> --json id --jq '.id')

# Add the child as a sub-issue of the parent
gh api graphql -f query='
  mutation($parentId: ID!, $childId: ID!) {
    addSubIssue(input: { issueId: $parentId, subIssueId: $childId }) {
      issue { number }
      subIssue { number }
    }
  }
' -f parentId="$PARENT_ID" -f childId="$CHILD_ID"
```

Repeat step 3 for each child issue. A `Part of #N` line alone is not a link: GitHub tracks only the sub-issue relationship.

**Every child is a linked sub-issue, whenever it is filed.** That includes an issue filed later under an existing epic, and one split out of an issue during implementation, review or user testing. Link it with step 3 when it is created.

**Track work as sub-issues, not checklist lines.** Any work an epic still needs must exist as an open sub-issue: a step left only as a checklist line in the parent's body is invisible to the epic's progress. Also add a new issue to the master index issue's roadmap.

## Step 7: Create the Issue

Create the GitHub Issue using `gh issue create`. Always include the `--label` and `--milestone` flags.

```
gh issue create \
  --title "<concise title, under 70 chars>" \
  --label "<type>" --label "<area>" --label "<priority>" \
  --milestone "<milestone title>" \
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

- [ ] `make lint-check` passes with zero issues
- [ ] `make test`, `make cover`, `make test-e2e` pass (plus `make test-e2e-docker` / `make lint-nf` / `make bench` when the emitter or data plane changes)
- [ ] `make test-mrp-diff` passes against real `mrp`

## Risks

- <Any risks, unknowns, or decisions needing input>

EOF
)"
```

After creating the issue, display the issue URL, the labels applied, and a brief summary of what was filed.
