"""Shared helpers for the AWS Batch run analysis: the run directory, the Nextflow trace, task keys.

A run directory holds one Batch run's inputs under fixed names (fetch_run.sh writes them) and the files each
step derives from them; README.md lists both. Times are compared in local time, as the trace and .nextflow.log
record them, so run every step under the TZ of the host that ran Nextflow.
"""
import csv
import datetime as dt
import json
import os
import re

HASH_DIR = r'[0-9a-f]{2}/[0-9a-f]{30}'

DURATION_UNITS = {'s': 1, 'm': 60, 'h': 3600, 'd': 86400}
SIZE_UNITS = {'B': 1, 'KB': 1024, 'MB': 1024 ** 2, 'GB': 1024 ** 3, 'TB': 1024 ** 4}


def duration(s):
    """Seconds in a trace duration such as '1m 55s' or '646ms'; None for '-'."""
    s = s.strip()
    if s in ('-', ''):
        return None
    return sum(float(v) / 1000 if u == 'ms' else float(v) * DURATION_UNITS[u]
               for v, u in re.findall(r'([\d.]+)(ms|h|m|s|d)', s))


def size(s):
    """Bytes in a trace size such as '2.8 MB'."""
    s = s.strip()
    if s in ('-', '', '0'):
        return 0
    v, u = s.split()
    return float(v) * SIZE_UNITS[u]


def load_trace(path):
    """The trace's rows, with submit, duration, realtime and end parsed, and the process's short name."""
    rows = []
    with open(path) as f:
        for r in csv.DictReader(f, delimiter='\t'):
            r['sub'] = dt.datetime.strptime(r['submit'], '%Y-%m-%d %H:%M:%S.%f')
            r['dur'] = duration(r['duration'])
            r['rt'] = duration(r['realtime'])
            r['end'] = r['sub'] + dt.timedelta(seconds=r['dur'] or 0)
            r['short'] = r['name'].split(' (')[0].split(':')[-1]
            rows.append(r)
    return rows


def task_key(t):
    """The process's short name, with the chunk for a split stage's main: one key per task, stable across runs."""
    m = re.search(r'(chnk\d+)\)$', t['name'])
    return t['short'] + ('/' + m.group(1) if m else '')


def work_dir(tasks):
    """The run's -work-dir URI: every task's work dir less its two hash directories."""
    roots = {re.sub(rf'/{HASH_DIR}/?$', '', t['workdir']) for t in tasks}
    if len(roots) != 1:
        raise ValueError(f'work dir: the tasks name {len(roots)} work dirs: {sorted(roots)[:3]}')
    return roots.pop()


def rel_dir(root, task_workdir):
    """A task's work dir relative to the run's work dir: '<2 hex>/<30 hex>'."""
    return task_workdir.rstrip('/')[len(root) + 1:]


def s3_key(uri):
    """The object key of an s3:// URI."""
    return uri.split('/', 3)[3]


def s3_listing(run_dir):
    """`aws s3 ls --recursive` lines of the run's work dir as (key, LastModified, size); [] when absent."""
    path = os.path.join(run_dir, 's3ls.txt')
    if not os.path.exists(path):
        return []
    out = []
    for line in open(path):
        p = line.split(None, 3)
        if len(p) == 4:
            out.append((p[3].rstrip('\n'), dt.datetime.fromisoformat(f'{p[0]} {p[1]}'), int(p[2])))
    return out


def objects_by_task(run_dir, root):
    """Work-dir objects per task dir: {'<2 hex>/<30 hex>': [(name in the task dir, LastModified, size)]}."""
    pat = re.compile(rf'{re.escape(s3_key(root))}/({HASH_DIR})/(.+)')
    by = {}
    for k, ts, n in s3_listing(run_dir):
        m = pat.match(k)
        if m:
            by.setdefault(m.group(1), []).append((m.group(2), ts, n))
    return by


def label(run_dir):
    """A run's label in cross-run tables: its directory's name."""
    return os.path.basename(os.path.normpath(run_dir))


def path(run_dir, name):
    return os.path.join(run_dir, name)


def read_json(run_dir, name):
    with open(path(run_dir, name)) as f:
        return json.load(f)


def write_json(run_dir, name, obj, **kw):
    with open(path(run_dir, name), 'w') as f:
        json.dump(obj, f, **kw)


def write_csv(file, rows):
    with open(file, 'w', newline='') as f:
        w = csv.DictWriter(f, fieldnames=list(rows[0]))
        w.writeheader()
        w.writerows(rows)


def iso(s):
    """A naive local datetime from an ISO string, converting one with a zone ('Z' or an offset) to local time."""
    d = dt.datetime.fromisoformat(s.replace('Z', '+00:00'))
    return d.astimezone().replace(tzinfo=None) if d.tzinfo else d


def tasks_by_id(run_dir):
    return {t['task_id']: t for t in read_json(run_dir, 'tasks.json')}


def instance_id(arn):
    """The container-instance ID in a containerInstanceArn."""
    return (arn or '').split('/')[-1]
