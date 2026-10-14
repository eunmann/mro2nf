#!/usr/bin/env python3
"""Walk a run's critical path back from OUTS and split it into dispatch, queue, nc, compute (rt) and poll.

From OUTS, the path steps to the producer that finished last. Where a task was submitted more than 5 s after
that producer finished, a value dependency (such as a preflight gate) bound its submit, and the path steps to
the last task to finish before the submit instead. PLAN ends the walk.

  dispatch = Batch createdAt - the previous path task's end (Nextflow noticing it, and submitting the next)
  queue, nc, rt, poll as phases.py computes them; the five sum to the path's wall time

--end log (default) ends each task at its .nextflow.log completion (ms resolution); --end trace at trace
submit + duration (whole seconds above a minute).

usage: cp.py [--end log|trace] RUN_DIR   (reads tasks.json, stagein.json; writes cp_log.json or cp.json)
"""
import argparse
import datetime as dt
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import perfrun

COMPONENTS = ('dispatch', 'queue', 'nc', 'rt', 'poll')
P = dt.datetime.fromisoformat


def walk(tasks, si):
    root = next(t['task_id'] for t in tasks.values() if t['short'] == 'OUTS')
    plan = next(t['task_id'] for t in tasks.values() if t['short'] == 'PLAN')
    path, cur = [], root
    while cur:
        path.append(cur)
        if cur == plan:
            break
        prods = [p for p in si[cur]['producers'] if p != '?']
        nxt = max(prods, key=lambda p: P(tasks[p]['end'])) if prods else plan
        sub = P(tasks[cur]['sub'])
        if (sub - P(tasks[nxt]['end'])).total_seconds() > 5:
            nxt = max((t for t in tasks.values() if P(t['end']) <= sub), key=lambda t: P(t['end']))['task_id']
            print('value-dep link', cur, '<-', nxt)
        cur = nxt
    return path[::-1]


def rows(tasks, si, path, detail):
    t0 = min(P(t['sub']) for t in tasks.values())
    out, prev_end = [], None
    for tid in path:
        t = tasks[tid]
        disp = (P(t['created']) - P(prev_end or t0.isoformat())).total_seconds()
        r = dict(id=tid, name=t['short'], sub=(P(t['sub']) - t0).total_seconds(), end=(P(t['end']) - t0).total_seconds(),
                 dispatch=disp, queue=t['queue'], nc=t['nc'], rt=t['rt'] or 0, poll=t['poll'], nin=len(si[tid]['inter']))
        if detail:
            r.update(itype=t.get('itype'), bytes_in=si[tid].get('bytes_in'), bytes_out=si[tid].get('bytes_out'))
        out.append(r)
        prev_end = t['end']
    return out, (P(tasks[path[-1]]['end']) - t0).total_seconds()


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument('--end', choices=('log', 'trace'), default='log')
    ap.add_argument('run_dir')
    a = ap.parse_args()
    tasks = perfrun.tasks_by_id(a.run_dir)
    si = perfrun.read_json(a.run_dir, 'stagein.json')
    if a.end == 'log':
        for t in tasks.values():
            t['end'], t['poll'] = t['end_log'], t['poll_log']
    cp, wall = rows(tasks, si, walk(tasks, si), a.end == 'log')
    print(f"{'id':>4} {'sub':>6} {'end':>6} {'disp':>5} {'queue':>6} {'nc':>6} {'rt':>6} {'poll':>5} {'nin':>3} task")
    for r in cp:
        print(f"{r['id']:>4} {r['sub']:6.0f} {r['end']:6.0f} {r['dispatch']:5.1f} {r['queue']:6.1f} {r['nc']:6.1f} "
              f"{r['rt']:6.1f} {r['poll']:5.1f} {r['nin']:3d} {r['name']}")
    tot = {k: sum(r[k] for r in cp) for k in COMPONENTS}
    print('tasks on path', len(cp), 'wall', wall, 'sum', sum(tot.values()))
    for k, v in tot.items():
        print(f"  {k:9s} {v:8.1f} s  {100*v/wall:5.1f}%")
    perfrun.write_json(a.run_dir, 'cp_log.json' if a.end == 'log' else 'cp.json', cp, indent=1)
    print('all tasks', len(tasks), *(f'sum {k} {sum((t[k] or 0) for t in tasks.values())}'
                                     for k in ('incont', 'rt', 'nc', 'queue', 'dur', 'poll')))


if __name__ == '__main__':
    main()
