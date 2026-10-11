---
description: Bounded review-fix loop for the current branch — one full review, then short checks of the fixes, at most three rounds. Use after implementing a change and before creating a PR. This is the primary review command — use it instead of manually reviewing code or the built-in /code-review.
---

# Review Loop

Find and fix what a careful reviewer would block the merge on, at a bounded cost.

**What counts.** The bar is CLAUDE.md's North star: the Martian stage ABI, and pipeline outputs identical to `mrp` every run (deterministic, `-resume`-stable, loud failures), at orchestration cost that never scales with fork width or chunk count. A finding names who hits it (a pipeline author, an operator launching runs, a downstream stage) on a reachable path, and how they notice. Theoretical findings are refuted, not fixed. Every fix preserves the intended behaviour of the code it touches. When the intended behaviour is unclear (`mrp`, docs and tests disagree or are silent), the fixer skips the finding and states the concrete alternatives, and the coordinator asks the user.

**Why it is bounded.** Measured over 50 rounds on four branches of another repo with this loop: max-effort full-round finders averaged 169 turns at a 243k-token peak context and spent 78% of all tokens; three quarters of the findings were low severity, and a second full review of unchanged code produced new low findings every time rather than converging. So the loop reviews the branch once, then reviews only what its own fixes changed, and low findings never buy another round.

## Rounds

| Round | Range | Finders | Mode |
|---|---|---|---|
| 1 — full | `$(git merge-base main HEAD)..HEAD` | 1 per ~1500 changed code lines (tests not counted) from `review-scope.sh`, at most 3 | full |
| 2, 3 — check | the previous round's reviewed tip`..HEAD` (that round's fixes) | 1 | check |

- **Continue** only if the round confirmed a `high` or `medium` finding. Low findings are fixed in the same round and end it.
- **Stop** after round 3 regardless. Anything still open goes to the user with its evidence; a fourth round runs only if the user asks for it.
- Never re-review code an earlier round already reviewed and no fix has touched since.

## Round procedure

State lives in `$(git rev-parse --git-dir)/review-loop/` (inside the git dir, so per worktree and never committed): `reviewed` holds the tip the last round reviewed.

1. **Preflight.** `git status --porcelain` empty.
2. **Scope.** The base is the merge-base in round 1 and `$(cat "$(git rev-parse --git-dir)/review-loop/reviewed")` after it; then `mkdir -p "$(git rev-parse --git-dir)/review-loop" && git rev-parse HEAD > "$(git rev-parse --git-dir)/review-loop/reviewed"`. Round 1 only: `.claude/scripts/review-scope.sh <base>` prints changed code and test lines per directory, goldens and vendored paths excluded. Group the directories into finder groups along package boundaries, keeping a package's tests with it (≤ 1500 code lines → one group).
3. **Run the round as one Workflow**: `.claude/workflows/review-round.js` with `args` `{round, mode: "full"|"check", range: "<base>..<tip>", groups: [{name, paths}], trailer, checkout}` (`checkout` is the absolute path of the branch's worktree). For a check round, one group with `paths: ["."]`. The finders return findings; the script deduplicates them; one `review-fixer` verifies and fixes every finding in that checkout. The coordinator reads only the workflow's result.
4. **Gate**, once: `make lint-check`, and `make test` only if step 6 continues the loop (the final gate covers the last round). A failure is fixed before anything else.
5. **Ask** any skipped finding's question to the user (`AskUserQuestion`).
6. **Next.** Apply the continue/stop rule above. Print the round line.

## Final gate

Only the fast checks run locally. PR Validation on GitHub is the full gate: e2e with the mrp differential, docker, lint-nf, bench, cover and `-race`.

1. `make test`, plus targeted e2e (`go test -tags e2e -count=1 -run '<suites>' ./test/e2e/`) for the fixtures the fixes touched. Keep each run under about 4 minutes.
2. CDK changes: `cdk synth` emits the stack.
3. Push; update the PR body if the loop changed what the PR does; delete `$(git rev-parse --git-dir)/review-loop/`.
4. Merge only on a green PR Validation run on the final head. On a failure, read only the failing job's log tail (`gh run view <id> --log-failed | tail -n 100`).

Export `JAVA_HOME` for the Nextflow suites when Java is not on `PATH`.

## Commits

The fixer commits one logical change at a time (related findings together), in the `workflow` skill's Commit Messages format, labelled `Task: review R<N>`. The PR is squash-merged, so branch commits exist for review, not for `main`'s history; don't split a fix finer than a reviewer needs.

## Output

Per round, one line:

```
Round N (full|check, range X..Y): findings F (high H, medium M, low L) -> fixed K, refuted R, asked Q | next: check|stop
```

At the end: rounds run, totals by severity, and the final gate's local results.
