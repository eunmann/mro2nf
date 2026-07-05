#!/usr/bin/env python3
"""Print a benchmark metrics table and gate on data movement and overhead.

Reads the run's metrics (one JSON object per line) and a committed baseline,
then applies two independent gates:

  * baseline gate -- per lane, `refs` (the transfer multiplier), `plumbing_tasks`,
    and `tasks` must not exceed the committed baseline: a rise is respectively a
    reintroduced per-hop re-transfer, reintroduced data-plane bookkeeping, or
    extra task executions. Equal is fine; the target is monotone improvement.
  * scaling gate -- lanes that run one fixture at two widths (`<base>_wN` /
    `<base>_cN`, optionally `_native`) must report an IDENTICAL plumbing task
    count at every width: constant orchestration overhead is acceptable,
    overhead that scales with fork width or chunk count is a bug (the CLAUDE.md
    overhead rule). This gate compares the current run against itself, so it
    holds even when re-baselining.

With BENCH_UPDATE=1 it rewrites the baseline from the current run instead of
gating on it (used to record a new, lower baseline after a data-plane
improvement lands) — but still refuses to record a baseline that violates the
scaling gate.
"""
import json
import os
import re
import sys

# Baseline-gated metrics: any increase over the committed baseline is a
# regression. `tasks` counts unique task identities, not trace rows — a
# transient executor retry re-runs a task under the same name and
# bench_metrics.py keeps only the final attempt, so a retry on an unrelated
# task cannot trip this gate (see bench_metrics.parse_trace).
GATED = ("refs", "plumbing_tasks", "tasks")

# A lane name of one fixture run at a given width: <base>_w<N> (fork width) or
# <base>_c<N> (chunk count), with an optional _native suffix. All widths of one
# (base, mode) pair form a scaling group.
_WIDTH = re.compile(r"^(?P<base>.+)_[wc]\d+(?P<native>_native)?$")

# Lanes that intentionally run at a single width and therefore carry no
# scaling group. Every lane must either match _WIDTH or be listed here:
# anything else is a naming mistake that would silently escape the scaling
# gate, so check_scaling fails it loudly instead.
SINGLE_WIDTH_LANES = frozenset({"chain", "chain_native"})


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
    # plumb = tasks that add no Martian stage (BIND/FORK/MERGE/DISABLE/PUBLISH/
    # entry) — the data-plane machinery Nextflow adds beyond Martian's DAG.
    return (
        f"tasks={m['tasks']:>3} (stage={m.get('stage_tasks', 0):>2} "
        f"plumb={m.get('plumbing_tasks', 0):>2})  procs={m['processes']:>2}  "
        f"refs={m['refs']:>3}  mult={m['multiplier']:<6}  wchar={m['wchar_bytes']:>9}"
    )


def scaling_group(name):
    m = _WIDTH.match(name)
    if not m:
        return None
    return m.group("base") + (m.group("native") or "")


def check_scaling(cur):
    """Fail (rc 1) if any width group's plumbing task count varies with width.

    Also fails on the two ways a lane could silently escape this gate: a lane
    name that neither parses as <base>_[wc]<N>[_native] nor is an allow-listed
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
                f"<base>_[wc]<N>[_native] nor an allow-listed single-width lane "
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
        counts = {n: cur[n]["plumbing_tasks"] for n in groups[g]}
        if len(set(counts.values())) > 1:
            print(
                f"SCALING[{g}]: plumbing_tasks must be identical at every width "
                f"(constant overhead only), got {counts}"
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
        if check_scaling(cur):
            print("refusing to record a baseline that violates the scaling gate")
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
    if rc == 0:
        print(
            "OK: no regression vs baseline (refs/plumbing_tasks/tasks); "
            "plumbing is width-flat"
        )
    return rc


if __name__ == "__main__":
    sys.exit(main())
