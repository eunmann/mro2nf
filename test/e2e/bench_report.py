#!/usr/bin/env python3
"""Print a benchmark metrics table and gate on data movement and overhead.

Reads the run's metrics (one JSON object per line) and a committed baseline. In
report mode it prints the metrics next to the baseline and fails if any
benchmark's `refs` (and thus its transfer multiplier), `plumbing_tasks`, or
`tasks` exceeds the baseline — respectively a reintroduced per-hop re-transfer,
reintroduced data-plane bookkeeping, or extra task executions. Equal is fine;
the target is monotone improvement. With BENCH_UPDATE=1 it rewrites the
baseline from the current run instead of gating (used to record a new, lower
baseline after a data-plane improvement lands).
"""
import json
import os
import sys

# Baseline-gated metrics: any increase over the committed baseline is a
# regression.
GATED = ("refs", "plumbing_tasks", "tasks")


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


def main():
    metrics_path, baseline_path = sys.argv[1], sys.argv[2]
    cur = load_jsonl(metrics_path)

    if os.environ.get("BENCH_UPDATE") == "1":
        with open(baseline_path, "w") as h:
            json.dump({k: cur[k] for k in sorted(cur)}, h, indent=2)
            h.write("\n")
        print(f"baseline updated: {baseline_path}")
        for name in sorted(cur):
            print(f"  {name:<8} {fmt(cur[name])}")
        return 0

    base = load_baseline(baseline_path)
    rc = 0
    print(f"{'benchmark':<10} {'current':<95}")
    for name in sorted(cur):
        m = cur[name]
        print(f"{name:<10} {fmt(m)}")
        b = base.get(name)
        if b is None:
            print(f"           (no baseline; run BENCH_UPDATE=1 make bench to record)")
            continue
        print(f"{'  baseline':<10} {fmt(b):<95}")
        for metric in GATED:
            if m[metric] > b[metric]:
                print(f"  REGRESSION[{name}]: {metric} {m[metric]} > baseline {b[metric]}")
                rc = 1
    if rc == 0:
        print("OK: no regression vs baseline (refs/plumbing_tasks/tasks)")
    return rc


if __name__ == "__main__":
    sys.exit(main())
