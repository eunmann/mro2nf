#!/usr/bin/env python3
"""Print a benchmark metrics table and gate on data movement and overhead.

Reads the run's metrics (one JSON object per line) and a committed baseline,
then applies four independent gates:

  * baseline gate -- per lane, `refs` (the transfer multiplier), `plumbing_tasks`,
    and `tasks` must not exceed the committed baseline: a rise is respectively a
    reintroduced per-hop re-transfer, reintroduced data-plane bookkeeping, or
    extra task executions. Equal is fine; the target is monotone improvement.
  * scaling gate -- lanes that run one fixture at two widths (`<base>_wN` /
    `<base>_cN`) must report an IDENTICAL plumbing task
    count at every width: constant orchestration overhead is acceptable,
    overhead that scales with fork width or chunk count is a bug (the CLAUDE.md
    overhead rule). This gate compares the current run against itself, so it
    holds even when re-baselining. A group whose lanes carry `known_scaling`
    (the open issue behind its per-fork plumbing) may differ by width — the
    baseline gate still holds each width at or below its recorded count — but
    fails once it is flat, so the marker is removed when the issue is fixed.
    A group whose lanes carry `flat_staging` must also stage, into its
    largest stage task, the same bytes at every width (within STAGING_SLACK):
    a fork stages its own share of the data, never all N forks' shares.
    Every group must write, per producer, the same `restaged_bytes` at every
    width (within STAGING_SLACK): a file every fork reads is referenced
    where it was staged, not copied into each fork, by a non-split stage
    (#498) or a split stage's tasks (#507). And no group's stage tasks may
    declare, as outputs an object-store work dir uploads, more bytes per
    byte their jobs wrote at a wider width (`output_bytes` / `job_bytes`,
    within STAGING_SLACK): a file is uploaded once, not once more per fork
    (#623).
  * mrp-jobs gate -- every lane's `stage_tasks` must equal `mrp_jobs`, the
    jobs real mrp runs for the same invocation: one Nextflow task per Martian
    job, so stage work is neither duplicated nor hidden in plumbing.
  * staged gate -- every lane's `staged_bytes` must be above 0: a lane that
    staged nothing was not measured, and the staging gates would pass on
    zeros.
  * publish gate -- every lane's `plumbing_probe_refs` must be 0: a published
    stage file is published from the task that wrote it, so no plumbing task
    stages or writes its bytes (#610).

With BENCH_UPDATE=1 it rewrites the baseline from the current run instead of
gating on it (used to record a new, lower baseline after a data-plane
improvement lands) — but still refuses to record a baseline that violates the
scaling, mrp-jobs or staged gate, or that raises a known-scaling lane's
plumbing.
"""
import json
import os
import re
import sys

# Baseline-gated metrics: any increase over the committed baseline is a
# regression. `tasks` counts unique task identities, not trace rows (see
# bench_metrics.parse_trace); a run with a retried attempt fails in the
# harness before it reaches this gate (runNextflow, #269).
GATED = ("refs", "plumbing_tasks", "tasks")

# A lane name of one fixture run at a given width: <base>_w<N> (fork width) or
# <base>_c<N> (chunk count). All widths of one base form a scaling group.
_WIDTH = re.compile(r"^(?P<base>.+)_[wc]\d+$")

# Lanes that intentionally run at a single width and therefore carry no
# scaling group. Every lane must either match _WIDTH or be listed here:
# anything else is a naming mistake that would silently escape the scaling
# gate, so check_scaling fails it loudly instead.
SINGLE_WIDTH_LANES = frozenset({"chain"})

# A fork element's records grow with the digits of its index and fork count,
# so a width-flat task's staged bytes still differ by a few bytes; a share
# that grows with width grows by the width ratio.
STAGING_SLACK = 1.05


def load_jsonl(path):
    out = {}
    with open(path) as h:
        for line in h:
            line = line.strip()
            if line:
                m = json.loads(line)
                out[m["name"]] = m
    return out


def load_baseline(path):
    if not os.path.exists(path):
        return {}
    with open(path) as h:
        return json.load(h)


def fmt(m):
    # plumb = tasks that run no Martian stage phase — the data-plane machinery
    # Nextflow adds beyond Martian's DAG.
    return (
        f"tasks={m['tasks']:>3} (stage={m.get('stage_tasks', 0):>2} "
        f"plumb={m.get('plumbing_tasks', 0):>2} mrp={m.get('mrp_jobs', '-'):>2})  "
        f"procs={m['processes']:>2}  refs={m['refs']:>3}  mult={m['multiplier']:<6}  "
        f"wchar={m['wchar_bytes']:>9}  restaged={m.get('restaged_bytes', '-'):>8}"
    )


def scaling_group(name):
    m = _WIDTH.match(name)
    if not m:
        return None
    return m.group("base")


def lane_width(name):
    """The width N of a <base>_[wc]<N> lane."""
    return int(re.search(r"(\d+)$", name).group(1))


def check_scaling(cur):
    """Fail (rc 1) if any width group's plumbing task count varies with width.

    Also fails on the two ways a lane could silently escape this gate: a lane
    name that neither parses as <base>_[wc]<N> nor is an allow-listed
    single-width lane (a typo'd lane would otherwise skip the check), and a
    width group left with fewer than two widths (one width proves nothing
    about flatness).
    """
    groups = {}
    rc = 0
    for name in sorted(cur):
        g = scaling_group(name)
        if g is not None:
            groups.setdefault(g, []).append(name)
        elif name not in SINGLE_WIDTH_LANES:
            print(
                f"SCALING[{name}]: unrecognized bench lane naming — neither "
                f"<base>_[wc]<N> nor an allow-listed single-width lane "
                f"({', '.join(sorted(SINGLE_WIDTH_LANES))}); a mis-named lane "
                f"would silently skip the width-flatness gate"
            )
            rc = 1
    for g in sorted(groups):
        if len(groups[g]) < 2:
            print(
                f"SCALING[{g}]: only one width ({groups[g][0]}) in the run — "
                f"the width-flatness gate needs at least two widths per group"
            )
            rc = 1
            continue
        lanes = {n: cur[n] for n in groups[g]}
        rc = check_group(g, lanes) or rc
        rc = check_staging(g, lanes) or rc
        rc = check_restaged(g, lanes) or rc
        rc = check_outputs(g, lanes) or rc
    return rc


def check_outputs(g, lanes):
    """Gate a group's declared-output bytes per byte its jobs wrote (rc 1 on
    failure).

    The data a fixture's jobs write may grow with width; what the stage
    tasks declare as outputs, which an object-store work dir uploads, may
    not grow faster: a file written into one more output per fork, or an
    element carrying every fork's files, raises the ratio with the width.
    """
    if any(not m.get("output_bytes") or not m.get("job_bytes") for m in lanes.values()):
        print(
            f"OUTPUTS[{g}]: a width measured no declared-output or job bytes; "
            f"bench_metrics.py (declared_outputs, job_bytes) no longer reads them"
        )
        return 1
    per = {n: round(m["output_bytes"] / m["job_bytes"], 4) for n, m in lanes.items()}
    by_width = [per[n] for n in sorted(per, key=lane_width)]
    if any(wide > narrow * STAGING_SLACK for narrow, wide in zip(by_width, by_width[1:])):
        print(
            f"OUTPUTS[{g}]: declared-output bytes per byte the jobs wrote must "
            f"not grow with width (slack {STAGING_SLACK}), got {per}"
        )
        return 1
    return 0


def check_restaged(g, lanes):
    """Gate a group's restaged bytes per producer (rc 1 on failure).

    Normalized per fork they would read flat even when every fork copies a
    broadcast file; per producer, the unit of the data, they grow with width.
    """
    per = {n: m.get("restaged_bytes", 0) / m["producers"] for n, m in lanes.items()}
    if max(per.values()) > min(per.values()) * STAGING_SLACK:
        print(
            f"RESTAGED[{g}]: bytes tasks write that copy a file they staged, per "
            f"producer, must not grow with width (slack {STAGING_SLACK}), got {per}"
        )
        return 1
    return 0


def check_staging(g, lanes):
    """Gate a flat_staging group's largest per-task staging (rc 1 on failure)."""
    flagged = {bool(m.get("flat_staging")) for m in lanes.values()}
    if len(flagged) > 1:
        print(f"STAGING[{g}]: widths disagree on flat_staging")
        return 1
    if not flagged.pop():
        return 0
    peaks = {n: max(m.get("stage_staged_bytes_max", {}).values(), default=0) for n, m in lanes.items()}
    if max(peaks.values()) > min(peaks.values()) * STAGING_SLACK:
        print(
            f"STAGING[{g}]: the most bytes one stage task stages must not grow "
            f"with width (slack {STAGING_SLACK}), got {peaks}"
        )
        return 1
    return 0


def check_group(g, lanes):
    """Gate one width group's plumbing counts (rc 1 on failure)."""
    counts = {n: m["plumbing_tasks"] for n, m in lanes.items()}
    flat = len(set(counts.values())) == 1
    known = {m.get("known_scaling") for m in lanes.values()}
    if len(known) > 1:
        print(f"SCALING[{g}]: widths disagree on known_scaling: {sorted(map(str, known))}")
        return 1
    issue = known.pop()
    if issue is None:
        if not flat:
            print(
                f"SCALING[{g}]: plumbing_tasks must be identical at every width "
                f"(constant overhead only), got {counts}"
            )
            return 1
        return 0
    if flat:
        print(
            f"SCALING[{g}]: marked known_scaling #{issue} but plumbing_tasks is "
            f"now width-flat {counts}; drop knownScaling from its lanes in "
            f"test/e2e/bench_test.go and re-record with BENCH_UPDATE=1 make bench"
        )
        return 1
    print(
        f"KNOWN-SCALING[{g}]: plumbing_tasks grows with width (#{issue}), got "
        f"{counts}; the baseline gate holds each width at its recorded count"
    )
    return 0


def check_mrp_jobs(cur):
    """Fail (rc 1) unless every lane's stage tasks equal mrp's job count."""
    rc = 0
    for name in sorted(cur):
        m = cur[name]
        if m.get("stage_tasks") != m.get("mrp_jobs"):
            print(
                f"MRP-JOBS[{name}]: stage_tasks {m.get('stage_tasks')} != mrp_jobs "
                f"{m.get('mrp_jobs')}; each Martian job must be exactly one stage "
                f"task (stage process names: test/e2e/bench_test.go writeStageProcesses)"
            )
            rc = 1
    return rc


def check_staged(cur):
    """Fail (rc 1) unless every lane staged some bytes into its tasks.

    Every fixture stages files between its tasks, so a lane that staged none
    was not measured: bench_metrics.py no longer reads the staging Nextflow
    writes, and the staging and restaged gates would pass on zeros.
    """
    rc = 0
    for name in sorted(cur):
        if not cur[name].get("staged_bytes"):
            print(
                f"STAGED[{name}]: no task staged anything; bench_metrics.py "
                f"(read_command_run) no longer reads this Nextflow's staging"
            )
            rc = 1
    return rc


def check_publish(cur):
    """Fail (rc 1) if a plumbing task staged or wrote a copy of the probe.

    OUTS lays out the published tree from the outs' metadata, and the
    workflow outputs publish each file from the task that wrote it, so the
    bytes of a published stage file reach no plumbing task.
    """
    rc = 0
    for name in sorted(cur):
        n = cur[name].get("plumbing_probe_refs", 0)
        if n:
            print(
                f"PUBLISH[{name}]: {n} plumbing task(s) staged or wrote a copy of "
                f"the probe file; a published file is published from the task "
                f"that wrote it, never through a plumbing task (#610)"
            )
            rc = 1
    return rc


def check_known_ratchet(cur, base):
    """Fail (rc 1) if re-recording would raise a known-scaling lane's plumbing.

    Re-recording otherwise writes whatever the run measured, and a scaling
    lane is exempt from the flatness check, so this is what keeps its per-fork
    plumbing from growing through BENCH_UPDATE=1.
    """
    rc = 0
    for name in sorted(cur):
        b = base.get(name)
        if "known_scaling" not in cur[name] or b is None:
            continue
        if cur[name]["plumbing_tasks"] > b["plumbing_tasks"]:
            print(
                f"KNOWN-SCALING[{name}]: plumbing_tasks {cur[name]['plumbing_tasks']} "
                f"> baseline {b['plumbing_tasks']}; known scaling may not get worse"
            )
            rc = 1
    return rc


def check_baseline(cur, base):
    """Print the metrics table and fail (rc 1) on any gated-metric increase.

    A lane present in only one of run/baseline is also a failure: a new lane
    without a baseline has no gate at all (record one with BENCH_UPDATE=1),
    and a baseline lane missing from the run means a lane was silently
    dropped — its gate vanished with it.
    """
    rc = 0
    print(f"{'benchmark':<18} current")
    for name in sorted(cur):
        m = cur[name]
        print(f"{name:<18} {fmt(m)}")
        b = base.get(name)
        if b is None:
            print(
                f"  NO-BASELINE[{name}]: lane has no committed baseline, so it "
                f"is ungated; run BENCH_UPDATE=1 make bench to record one"
            )
            rc = 1
            continue
        print(f"{'  baseline':<18} {fmt(b)}")
        for metric in GATED:
            if m[metric] > b[metric]:
                print(f"  REGRESSION[{name}]: {metric} {m[metric]} > baseline {b[metric]}")
                rc = 1
    for name in sorted(set(base) - set(cur)):
        print(
            f"  MISSING-LANE[{name}]: baseline lane absent from this run — a "
            f"dropped lane loses its gate; restore the lane or re-record the "
            f"baseline with BENCH_UPDATE=1 make bench"
        )
        rc = 1
    return rc


def main():
    metrics_path, baseline_path = sys.argv[1], sys.argv[2]
    cur = load_jsonl(metrics_path)

    if os.environ.get("BENCH_UPDATE") == "1":
        bad = check_scaling(cur)
        bad = check_mrp_jobs(cur) or bad
        bad = check_staged(cur) or bad
        bad = check_publish(cur) or bad
        bad = check_known_ratchet(cur, load_baseline(baseline_path)) or bad
        if bad:
            print("refusing to record a baseline that violates a gate above")
            return 1
        with open(baseline_path, "w") as h:
            json.dump({k: cur[k] for k in sorted(cur)}, h, indent=2)
            h.write("\n")
        print(f"baseline updated: {baseline_path}")
        for name in sorted(cur):
            print(f"  {name:<18} {fmt(cur[name])}")
        return 0

    rc = check_baseline(cur, load_baseline(baseline_path))
    rc = check_scaling(cur) or rc
    rc = check_mrp_jobs(cur) or rc
    rc = check_staged(cur) or rc
    rc = check_publish(cur) or rc
    if rc == 0:
        print(
            "OK: no regression vs baseline (refs/plumbing_tasks/tasks); "
            "plumbing is width-flat outside known-scaling groups; "
            "restaged bytes per producer are width-flat; "
            "declared-output bytes per job byte do not grow with width; "
            "stage tasks equal mrp jobs; every lane staged its inputs; "
            "no plumbing task moved a published file"
        )
    return rc


if __name__ == "__main__":
    sys.exit(main())
