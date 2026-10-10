---
description: "Implement a GitHub issue end-to-end: understand, plan, branch, implement, review, and open a PR. Use when given a GitHub issue number or URL to work on."
---

# Implement Issue

> **Mindset (CLAUDE.md Operating Principles):** Challenge assumptions — the issue's, the repo's, your own — with evidence from code. No guesses: verify by reading. Sprawl into related features before changing anything. Present options with tradeoffs; never commentary on size or difficulty. No hacks, no workarounds, no backwards-compatibility shims. Refactor when the code warrants it.

Take a GitHub issue from understanding through to a ready-to-merge PR.

**Issue:** $ARGUMENTS

## Current State

- **Branch:** !`git branch --show-current`
- **Status:** !`git status --short`
- **Recent commits:** !`git log --oneline -5`

## 1. Preflight

Ensure a clean starting point:

1. If there are uncommitted changes, warn the user and stop.
2. Switch to `main` and pull latest:
   ```
   git checkout main && git pull origin main
   ```

## 2. Understand the Issue

Fetch the issue: `gh issue view <number> --json title,body,labels,state,comments,assignees`.

Read the title, body, labels, and comments. Summarize:

- **Goal:** What does this issue ask for?
- **Motivation:** Why is it needed?
- **Constraints:** Requirements, edge cases, or non-goals mentioned.

### Still needed?

Check whether the issue is still relevant:

- Is it still open?
- Has it been partially addressed by recent commits? (`git log --oneline -20 --all`)
- Does the codebase already have the requested behavior? Search for key terms.

If the issue appears resolved or outdated, explain why and ask the user before continuing.

## 3. Plan

Apply the `repo-research` skill to this issue, at the depth it describes for the change.

During steps 4-5 (synthesis and task plan), also:

1. **Classify the work** — bug fix, new feature, refactor, or something else? This determines the implementation style (test-first only for bug fixes).
2. **Consider alternatives** — if there is a simpler or more robust approach than what the issue describes, explain clearly: what it is, why it is better, and the tradeoffs. The user should understand the options before committing to an approach. Pre-production: prefer the clean replacement over the compatible patch.
3. **Include warranted refactors** — if code adjacent to the change should be restructured for the fix to be clean, plan the refactor as its own commit rather than working around the existing structure.
4. **Break into atomic commits** — each step in the implementation plan should produce one atomic commit that compiles and passes tests independently.

Present the plan as a numbered list. Each item must name the file path, the change, and the rationale. Then write exactly:

> Approve this plan? Reply "approved" to proceed, or describe changes to adjust.

Do not proceed to branching or implementation until the user responds with explicit approval. Interpret ambiguous replies as "not yet" and ask a clarifying question.

## 4. Branch

After the plan is approved, create the feature branch:

```
git checkout -b feature/<short-name-from-issue>
```

Use a concise, descriptive branch name derived from the issue title.

## 5. Implement

If the issue changes what the emitter generates, transpile the affected fixtures before and after and read the generated `main.nf`/`modules/` diff. If it is an `mrp`-parity bug, reproduce it first under real `mrp` (`$MARTIAN_BIN`, default `~/workdir/martian/bin`) and record that output as the golden.

For each step in the plan:

1. **Implement code and tests together.** Table-driven with `t.Run`, standard `testing` package, `cmp.Diff` for structs. For **bug fixes only**, use the `tdd` skill: write the failing test first to prove the bug, then fix.
2. **Edit discipline.** Edit/Write tools for all source edits. Complete every edit in the step before any verification run.
3. **Verify once at the end of the step** — `make lint` (stage auto-fixes), then `make test`. All green.
4. **Commit** — one atomic commit per logical change, independently compilable and passing. Use the `workflow` skill's Commit Messages format, labelled `Task: impl #<issue-number>`.

## 6. Review

Run `/review-loop`. Its final gate runs the unit, coverage, e2e, lint-nf, bench and mrp-diff suites the branch reaches; do not open the PR until it passes.

## 7. Pull Request

1. Push the branch: `git push -u origin feature/<short-name>`.
2. Create the PR:

```
gh pr create --title "<type(scope): summary>" --body "$(cat <<'EOF'
Closes #<issue-number>

## Summary
- <what changed and why, 1-3 bullets>

## Test Plan
- <how to verify the changes>
EOF
)"
```

3. Copilot review: run `.claude/scripts/wait-copilot-review.sh <pr>` in the background, then address its threads with `/address-pr-comments` before the PR is merged.
4. Report the PR URL to the user.
