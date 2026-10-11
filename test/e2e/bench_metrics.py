#!/usr/bin/env python3
"""Collect data-movement metrics for one benchmark run.

Emits a single JSON object combining Nextflow's own reporting with a direct,
backend-agnostic measure of how many times a benchmark's probe file is staged
across the run's work dir:

  tasks       -- number of unique tasks (retried attempts collapsed; see parse_trace)
  stage_tasks -- tasks of a stage-phase process (the --stages list); every
                 other task is plumbing_tasks
  mrp_jobs    -- the jobs real mrp runs for the same invocation, which
                 stage_tasks must equal (bench_report.py gates it)
  processes   -- distinct process names (chunk tags collapsed)
  rchar/wchar -- aggregate characters read/written (trace; copy-volume proxy)
  edges       -- DAG edge count (-with-dag mermaid), the static process wiring
  refs        -- task work dirs into which the probe file is staged/materialized
  multiplier  -- refs / producers: the per-file transfer multiplier (ideal = 1)

`refs` is the portable stand-in for the true S3-object metric: on a shared FS
each staging is a cheap link, on S3 each is a transfer, but the count is the
same generated pipeline's shape either way. A file written once and carried
through k stages should trend toward multiplier 1 as the epic's data-plane work
lands; a regression that reintroduces per-hop re-transfer pushes it back up.
"""
import argparse
import json
import os
import subprocess

# Nextflow renders trace sizes with binary (1024) unit steps (KB/MB/GB); the
# absolute scale is irrelevant to the before/after comparison, only consistency.
_UNITS = {"B": 1, "KB": 1024, "MB": 1024**2, "GB": 1024**3, "TB": 1024**4}


def parse_size(s):
    s = (s or "").strip()
    if not s or s == "-":
        return 0.0
    parts = s.split()
    if len(parts) == 2:
        return float(parts[0]) * _UNITS.get(parts[1], 1)
    try:
        return float(s)
    except ValueError:
        return 0.0


def parse_trace(path, stages):
    """Aggregate the trace by unique task identity, not by row.

    The trace holds one row per ATTEMPT: under `errorStrategy retry` a
    transient failure adds a FAILED row and then a fresh row for the retry, so
    raw row counts would fail the `tasks` gate on any unrelated retry. The
    harness requests the default trace fields, which carry no attempt column —
    but the fully qualified task name (`WF:PROC (n)`; the generated processes
    set no `tag`, so the index makes each task's name unique) IS the stable
    task identity, and every attempt of one task repeats it. Keying rows by
    that name and letting the last attempt win therefore counts each task
    once, retried or not, and reads rchar/wchar from the attempt that
    actually completed.

    A task is plumbing unless its process is in `stages`, the stage-phase
    process names of the lane's program: an allowlist of plumbing names would
    count any new helper process as stage work.
    """
    by_name = {}
    with open(path) as h:
        header = h.readline().rstrip("\n").split("\t")
        idx = {c: i for i, c in enumerate(header)}
        for line in h:
            cols = line.rstrip("\n").split("\t")
            if len(cols) < len(header):
                continue
            by_name[cols[idx["name"]]] = cols  # last attempt wins
    tasks, plumb_tasks, procs, rchar, wchar = 0, 0, set(), 0.0, 0.0
    for name, cols in by_name.items():
        tasks += 1
        # Strip the workflow path prefix (A:B:PROC) and the chunk tag " (n)".
        base = name.split(" (")[0].split(":")[-1]
        procs.add(base)
        if base not in stages:
            plumb_tasks += 1
        if "rchar" in idx:
            rchar += parse_size(cols[idx["rchar"]])
        if "wchar" in idx:
            wchar += parse_size(cols[idx["wchar"]])
    plumbing = sorted(p for p in procs if p not in stages)
    return tasks, plumb_tasks, len(procs), int(rchar), int(wchar), plumbing


def count_edges(dag):
    if not dag or not os.path.exists(dag):
        return 0
    with open(dag) as h:
        return sum(1 for line in h if "-->" in line)


def count_refs(workdir, probe):
    """Count task work dirs that stage or materialize the probe file.

    Each immediate task dir (work/<xx>/<hash>) is searched once, following
    symlinks (grep -R), so a chunk that stages a shared bundle via a symlinked
    input dir is counted — that is exactly the per-task transfer S3 would incur.
    """
    root = os.path.join(workdir, "work")
    if not os.path.isdir(root):
        return 0
    refs = 0
    for two in sorted(os.listdir(root)):
        p2 = os.path.join(root, two)
        if not os.path.isdir(p2):
            continue
        for h in sorted(os.listdir(p2)):
            d = os.path.join(p2, h)
            if not os.path.isdir(d):
                continue
            r = subprocess.run(
                ["grep", "-RIsq", "-m1", "-e", probe, d],
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
            )
            if r.returncode == 0:
                refs += 1
    return refs


def parse_args():
    p = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    p.add_argument("--name", required=True, help="lane (baseline key)")
    p.add_argument("--trace", required=True, help="-with-trace TSV")
    p.add_argument("--workdir", required=True, help="dir holding the run's work/")
    p.add_argument("--probe", required=True, help="probe file marker string")
    p.add_argument("--producers", required=True, type=int, help="tasks that write the probe file")
    p.add_argument("--stages", required=True, help="JSON array of stage-phase process names")
    p.add_argument("--dag", default="", help="-with-dag mermaid file")
    p.add_argument("--mrp-jobs", required=True, type=int, help="jobs mrp runs for this invocation")
    p.add_argument(
        "--known-scaling",
        type=int,
        default=0,
        help="open issue whose plumbing grows with this lane's width (0: none)",
    )
    return p.parse_args()


def main():
    a = parse_args()
    with open(a.stages) as h:
        stages = frozenset(json.load(h))

    tasks, plumb_tasks, procs, rchar, wchar, plumbing = parse_trace(a.trace, stages)
    refs = count_refs(a.workdir, a.probe)
    mult = round(refs / a.producers, 3) if a.producers else float(refs)

    m = {
        "name": a.name,
        "tasks": tasks,
        # plumbing = tasks that run no Martian stage phase — the data-plane
        # machinery Nextflow adds beyond Martian's DAG.
        "plumbing_tasks": plumb_tasks,
        "stage_tasks": tasks - plumb_tasks,
        "mrp_jobs": a.mrp_jobs,
        "processes": procs,
        "stage_processes": procs - len(plumbing),
        "plumbing_processes": len(plumbing),
        "plumbing": plumbing,
        "edges": count_edges(a.dag),
        "rchar_bytes": rchar,
        "wchar_bytes": wchar,
        "refs": refs,
        "producers": a.producers,
        "multiplier": mult,
    }
    if a.known_scaling:
        m["known_scaling"] = a.known_scaling
    print(json.dumps(m))


if __name__ == "__main__":
    main()
