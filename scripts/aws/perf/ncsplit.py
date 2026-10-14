#!/usr/bin/env python3
"""Split each task's nc (Batch startedAt -> stoppedAt, less trace realtime) using its work dir's S3 object times.

  pre      = Batch startedAt -> .command.begin's LastModified (container start, wrapper start, first AWS CLI call)
  stage_in = .command.begin -> script start (nxf_stage: S3 downloads and links into the mount)
             script start = the first output object's LastModified - realtime
  unstage  = the first output object -> .command.out (nxf_unstage_outputs: S3 uploads)
  tail     = .command.out -> Batch stoppedAt (control-file uploads, container exit)
S3 LastModified is whole seconds, so each part carries +-1 s. A task with no output object is skipped.

usage: ncsplit.py [-o OUT] [--watch NAME,...] RUN_DIR...
       (reads tasks.json, stagein.json, s3ls.txt; writes OUT/ncsplit.csv; prints the tasks whose key contains a
        --watch name, and every task moving over 1 GB)
"""
import argparse
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import perfrun

P = perfrun.iso


def split(run, run_dir):
    tasks = perfrun.read_json(run_dir, 'tasks.json')
    root = perfrun.work_dir(tasks)
    objs = perfrun.objects_by_task(run_dir, root)
    si = perfrun.read_json(run_dir, 'stagein.json')
    rows = []
    for t in tasks:
        o = objs.get(perfrun.rel_dir(root, t['workdir']), [])
        named = {n: ts for n, ts, _ in o}
        outs = [ts for n, ts, _ in o if not n.split('/')[-1].startswith('.') and not n.endswith('/')]
        if '.command.begin' not in named or '.command.out' not in named or not outs:
            continue
        rt = t['rt'] or 0
        s_end = min(outs)
        started, stopped = P(t['started']), P(t['stopped'])
        rows.append(dict(run=run, id=t['task_id'], key=perfrun.task_key(t), itype=t['itype'], inst=t['inst'],
                         MB_in=round((si[t['task_id']].get('bytes_in') or 0) / 1e6),
                         MB_out=round((si[t['task_id']].get('bytes_out') or 0) / 1e6), nc=round(t['nc'], 1), rt=rt,
                         pre=round((named['.command.begin'] - started).total_seconds(), 1),
                         stage_in=round((s_end - named['.command.begin']).total_seconds() - rt, 1),
                         unstage=round((named['.command.out'] - s_end).total_seconds(), 1),
                         tail=round((stopped - named['.command.out']).total_seconds(), 1)))
    return rows


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument('-o', '--out', default='.')
    ap.add_argument('--watch', default='')
    ap.add_argument('run_dirs', nargs='+')
    a = ap.parse_args()
    watch = [w for w in a.watch.split(',') if w]
    rows = [r for d in a.run_dirs for r in split(perfrun.label(d), d)]
    if not rows:
        sys.exit('ncsplit: no task has .command.begin, .command.out and an output object in s3ls.txt')
    perfrun.write_csv(os.path.join(a.out, 'ncsplit.csv'), rows)
    print('| run | task | type | inst | MB in | MB out | nc | pre | stage-in | in MB/s | unstage | tail | rt |')
    print('|---|---|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|')
    for r in sorted(rows, key=lambda r: (r['key'], r['run'])):
        if any(k in r['key'] for k in watch) or r['MB_in'] + r['MB_out'] > 1000:
            mbs = round(r['MB_in'] / r['stage_in']) if r['stage_in'] > 0.5 and r['MB_in'] > 100 else ''
            print(f"| {r['run']} | {r['key']} | {r['itype']} | {r['inst']} | {r['MB_in']} | {r['MB_out']} | {r['nc']} | "
                  f"{r['pre']} | {r['stage_in']} | {mbs} | {r['unstage']} | {r['tail']} | {r['rt']} |")


if __name__ == '__main__':
    main()
