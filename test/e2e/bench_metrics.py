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

The staging probe reads every task work dir (see probe_task_dirs):

  staged_bytes / stage_staged_bytes -- bytes of the inputs Nextflow staged into
                 all tasks / into stage tasks: what an object-store backend
                 downloads
  stage_staged_bytes_max -- per stage process, the most bytes staged into
                 one of its tasks
  flat_staging -- set by --flat-staging: bench_report.py holds the largest
                 stage_staged_bytes_max to the same at every width
  uploaded_bytes / plumbing_uploaded_bytes -- bytes of the files all tasks /
                 plumbing tasks wrote into their work dirs, every hard link
                 counted: an upper bound on what an object-store backend
                 uploads, which is the declared outputs
  restaged_bytes -- bytes of the files tasks wrote that are a hard link to,
                 or a byte copy of, a file they staged: data written again
                 rather than referenced. bench_report.py holds it per
                 producer to the same at every width, so a file every fork
                 reads is not copied into each, by a non-split stage (#498)
                 or a split stage's tasks (#507)
  probe_writes -- files in task work dirs that are a copy of the probe file
                 (the ideal is producers: each producer writes it once)
  max_argv_bytes / max_arg_bytes -- the largest mre command line, and the
                 largest single argument on one, from the .mre-argv files
                 the scale tier's mre wrapper (scale_test.go writeArgvProbe)
                 writes

With --gc-logs the driver JVM's GC logs give driver_heap_bytes (the largest
heap left after a collection) and driver_heap_peak_bytes (the largest before
one); --wall-seconds is recorded as wall_seconds.

`refs` is the portable stand-in for the true S3-object metric: on a shared FS
each staging is a cheap link, on S3 each is a transfer, but the count is the
same generated pipeline's shape either way. A file written once and carried
through k stages should trend toward multiplier 1 as the epic's data-plane work
lands; a regression that reintroduces per-hop re-transfer pushes it back up.
"""
import argparse
import glob
import hashlib
import json
import os
import re
import shlex
import stat
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
    transient failure adds a FAILED row and then a fresh row for the retry.
    The e2e harness fails such a run (#269), but a trace read outside it must
    still count each task once.

    The fully qualified task name (`WF:PROC (n)`; the generated
    processes set no `tag`, so the index makes each task's name unique) is the
    stable task identity, and every attempt of one task repeats it. Keying
    rows by that name and letting the last attempt win therefore counts each
    task once, retried or not, and reads rchar/wchar from the attempt that
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


# The task name Nextflow writes into each .command.run header: the YAML-style
# header of recent releases, or the comment line of older ones.
_TASK_NAME = re.compile(r"^(?:### name: '(?P<yaml>.*)'|# NEXTFLOW TASK: (?P<old>.*))$")

# The files Nextflow itself writes into a task dir, which carry no task data.
_NEXTFLOW_FILES = re.compile(r"^\.command\.\w+$|^\.exitcode$")

# The argv probe file the -mre wrapper (scale_test.go writeArgvProbe) appends to.
ARGV_PROBE = ".mre-argv"


def task_dirs(workdir):
    root = os.path.join(workdir, "work")
    if not os.path.isdir(root):
        return
    for two in sorted(os.listdir(root)):
        p2 = os.path.join(root, two)
        if not os.path.isdir(p2):
            continue
        for h in sorted(os.listdir(p2)):
            d = os.path.join(p2, h)
            if os.path.isfile(os.path.join(d, ".command.run")):
                yield d


def read_command_run(d):
    """Return the task's process base name and the sources its staging links in.

    Nextflow writes the links into nxf_stage, or, once they reach
    NXF_WRAPPER_STAGE_FILE_THRESHOLD (1 MB), into .command.stage, which
    nxf_stage runs instead.
    """
    name, staging, in_stage = "", [], False
    with open(os.path.join(d, ".command.run")) as h:
        for line in h:
            line = line.rstrip("\n")
            m = _TASK_NAME.match(line)
            if m and not name:
                name = m.group("yaml") or m.group("old")
            if line.startswith("nxf_stage()"):
                in_stage = True
            elif in_stage and line == "}":
                break
            elif in_stage:
                staging.append(line)
    stage_file = os.path.join(d, ".command.stage")
    if os.path.exists(stage_file):
        with open(stage_file) as h:
            staging.extend(h)
    sources = [shlex.split(line.split("&& ")[-1])[2] for line in staging if "ln -s " in line]
    return name.split(" (")[0].split(":")[-1], sources


def tree_files(path):
    """Yield the file at path, or every file under it, links followed."""
    if not os.path.isdir(path):
        yield path
        return
    for parent, _, files in os.walk(path, followlinks=True):
        for f in files:
            yield os.path.join(parent, f)


def tree_bytes(path):
    """Bytes of the file at path, or of every file under it, links followed."""
    return sum(os.stat(p).st_size for p in tree_files(path))


def digest(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for block in iter(lambda: f.read(1 << 20), b""):
            h.update(block)
    return h.digest()


def restaged_bytes(written, sources):
    """Bytes of the written (path, size) files that are a hard link to, or a
    byte copy of, a file the task staged from sources.

    Only staged files of a size some written file has are hashed, so a task
    that copies nothing reads little.
    """
    ids, by_size = set(), {}
    for p in (p for s in sources for p in tree_files(s)):
        st = os.stat(p)
        ids.add((st.st_dev, st.st_ino))
        by_size.setdefault(st.st_size, []).append(p)
    sizes = {size for _, size in written if size in by_size}
    copies = {(size, digest(p)) for size in sizes for p in by_size[size]}
    total = 0
    for path, size in written:
        st = os.stat(path)
        if (st.st_dev, st.st_ino) in ids or (size in sizes and (size, digest(path)) in copies):
            total += size
    return total


def written_files(d):
    """Yield (path, size) of each regular file the task wrote under d.

    Symlinks are not followed: they are staged inputs, or outputs that point at
    them, and neither is a byte the task uploads. Each hard link is a separate
    file, as it is a separate object once uploaded.
    """
    for parent, dirs, files in os.walk(d):
        for f in files:
            if parent == d and (_NEXTFLOW_FILES.match(f) or f == ARGV_PROBE):
                continue
            p = os.path.join(parent, f)
            st = os.lstat(p)
            if stat.S_ISREG(st.st_mode):
                yield p, st.st_size
        dirs[:] = [x for x in dirs if not os.path.islink(os.path.join(parent, x))]


def is_probe_copy(path, probe):
    with open(path, "rb") as h:
        return h.read(len(probe)) == probe


def read_argv_probe(d):
    """Return the largest (argv, single argument) byte counts the wrapper logged."""
    path = os.path.join(d, ARGV_PROBE)
    if not os.path.exists(path):
        return 0, 0
    argv, arg = 0, 0
    with open(path) as h:
        for line in h:
            total, longest = (int(x) for x in line.split())
            argv, arg = max(argv, total), max(arg, longest)
    return argv, arg


def probe_task_dirs(workdir, probe, stages):
    """Aggregate the staging probe over every task dir of the run."""
    m = dict.fromkeys(
        (
            "staged_bytes",
            "stage_staged_bytes",
            "uploaded_bytes",
            "plumbing_uploaded_bytes",
            "restaged_bytes",
            "probe_writes",
            "max_argv_bytes",
            "max_arg_bytes",
        ),
        0,
    )
    m["stage_staged_bytes_max"] = {}
    marker = probe.encode()
    for d in task_dirs(workdir):
        name, sources = read_command_run(d)
        staged = sum(tree_bytes(s) for s in sources)
        written = list(written_files(d))
        uploaded = sum(size for _, size in written)
        m["probe_writes"] += sum(is_probe_copy(path, marker) for path, _ in written)
        m["restaged_bytes"] += restaged_bytes(written, sources)
        m["staged_bytes"] += staged
        m["uploaded_bytes"] += uploaded
        if name in stages:
            m["stage_staged_bytes"] += staged
            most = m["stage_staged_bytes_max"]
            most[name] = max(most.get(name, 0), staged)
        else:
            m["plumbing_uploaded_bytes"] += uploaded
        argv, arg = read_argv_probe(d)
        m["max_argv_bytes"] = max(m["max_argv_bytes"], argv)
        m["max_arg_bytes"] = max(m["max_arg_bytes"], arg)
    return m


# One HotSpot unified-logging GC line: "... 35M->12M(123M) 4.5ms".
_GC_PAUSE = re.compile(r"(\d+)([KMG])->(\d+)([KMG])\((\d+)[KMG]\)")
_GC_UNITS = {"K": 1024, "M": 1024**2, "G": 1024**3}


def driver_heap(gc_dir):
    """Return the largest heap after and before any GC across the driver's logs."""
    after, before = 0, 0
    for path in glob.glob(os.path.join(gc_dir, "*")):
        with open(path) as h:
            for m in _GC_PAUSE.finditer(h.read()):
                before = max(before, int(m.group(1)) * _GC_UNITS[m.group(2)])
                after = max(after, int(m.group(3)) * _GC_UNITS[m.group(4)])
    return after, before


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
    p.add_argument(
        "--flat-staging",
        action="store_true",
        help="the most bytes one stage task stages must not grow with this lane's width",
    )
    p.add_argument("--gc-logs", default="", help="dir of the driver JVM's -Xlog:gc files")
    p.add_argument("--wall-seconds", type=float, default=0, help="the run's wall time")
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
    m.update(probe_task_dirs(a.workdir, a.probe, stages))
    if a.gc_logs:
        m["driver_heap_bytes"], m["driver_heap_peak_bytes"] = driver_heap(a.gc_logs)
    if a.wall_seconds:
        m["wall_seconds"] = round(a.wall_seconds, 1)
    if a.known_scaling:
        m["known_scaling"] = a.known_scaling
    if a.flat_staging:
        m["flat_staging"] = True
    print(json.dumps(m))


if __name__ == "__main__":
    main()
