#!/usr/bin/env python3
"""Collect data-movement metrics for one benchmark run.

Emits a single JSON object combining Nextflow's own reporting with a direct,
backend-agnostic measure of how many times a benchmark's probe file is staged
across the run's work dir:

  tasks       -- number of task executions (trace rows)
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
import json
import os
import subprocess
import sys

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


def parse_trace(path):
    tasks, procs, rchar, wchar = 0, set(), 0.0, 0.0
    with open(path) as h:
        header = h.readline().rstrip("\n").split("\t")
        idx = {c: i for i, c in enumerate(header)}
        for line in h:
            cols = line.rstrip("\n").split("\t")
            if len(cols) < len(header):
                continue
            tasks += 1
            procs.add(cols[idx["name"]].split(" (")[0])
            if "rchar" in idx:
                rchar += parse_size(cols[idx["rchar"]])
            if "wchar" in idx:
                wchar += parse_size(cols[idx["wchar"]])
    return tasks, len(procs), int(rchar), int(wchar)


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


def main():
    name, trace, workdir, probe, producers = (
        sys.argv[1],
        sys.argv[2],
        sys.argv[3],
        sys.argv[4],
        int(sys.argv[5]),
    )
    dag = sys.argv[6] if len(sys.argv) > 6 else ""

    tasks, procs, rchar, wchar = parse_trace(trace)
    refs = count_refs(workdir, probe)
    mult = round(refs / producers, 3) if producers else float(refs)

    print(
        json.dumps(
            {
                "name": name,
                "tasks": tasks,
                "processes": procs,
                "edges": count_edges(dag),
                "rchar_bytes": rchar,
                "wchar_bytes": wchar,
                "refs": refs,
                "producers": producers,
                "multiplier": mult,
            }
        )
    )


if __name__ == "__main__":
    main()
