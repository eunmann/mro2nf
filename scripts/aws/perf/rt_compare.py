#!/usr/bin/env python3
"""Compare each task's compute (trace realtime) between two runs, with instance type, co-running tasks, rchar, wchar.

usage: rt_compare.py [-o OUT] RUN_DIR_A RUN_DIR_B
       (reads tasks.json; writes OUT/rt_compare.csv; markdown on stdout for tasks with rt >= 5 s in run A or B)
"""
import argparse
import datetime as dt
import os
import statistics as st
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import perfrun

P = dt.datetime.fromisoformat


def load(run_dir):
    ts = perfrun.read_json(run_dir, 'tasks.json')
    for t in ts:
        a, b = P(t['started']), P(t['stopped'])
        t['conc'] = sum(1 for x in ts if x is not t and x['inst'] == t['inst'] and P(x['started']) < b and P(x['stopped']) > a)
    return {perfrun.task_key(t): t for t in ts}


def summary(rows, lbl, sel):
    v = [r for r in rows if sel(r)]
    sa, sb = sum(r['a_rt'] for r in v), sum(r['b_rt'] for r in v)
    ratios = [r['b_rt'] / r['a_rt'] for r in v if r['a_rt'] >= 5]
    print(f"\n{lbl}: n={len(v)} sum rt A {sa:.0f} s, B {sb:.0f} s ({100*(sb-sa)/sa:+.1f}%)" +
          (f"; per-task B/A median {st.median(ratios):.3f}, min {min(ratios):.3f}, max {max(ratios):.3f}" if ratios else ''))


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument('-o', '--out', default='.')
    ap.add_argument('run_a')
    ap.add_argument('run_b')
    a = ap.parse_args()
    ka, kb = load(a.run_a), load(a.run_b)
    rows = []
    for k in sorted(ka, key=lambda k: P(ka[k]['sub'])):
        x, y = ka[k], kb.get(k)
        if y:
            rows.append(dict(key=k, a_type=x['itype'], a_conc=x['conc'], a_rt=x['rt'] or 0, b_type=y['itype'],
                             b_conc=y['conc'], b_rt=y['rt'] or 0, d_rt=round((y['rt'] or 0) - (x['rt'] or 0), 2),
                             rchar=y['rchar'], wchar=y['wchar'], same_type=x['itype'] == y['itype']))
    perfrun.write_csv(os.path.join(a.out, 'rt_compare.csv'), rows)
    A, B = perfrun.label(a.run_a), perfrun.label(a.run_b)
    print(f'A = run {A}, B = run {B}\n')
    print('| task | A type (conc) | A rt | B type (conc) | B rt | d rt | d % | rchar | wchar |')
    print('|---|---|---:|---|---:|---:|---:|---:|---:|')
    for r in rows:
        if max(r['a_rt'], r['b_rt']) >= 5:
            pct = format(100 * r['d_rt'] / r['a_rt'], '+.0f') + '%' if r['a_rt'] else '-'
            print(f"| {r['key']} | {r['a_type']} ({r['a_conc']}) | {r['a_rt']:.1f} | {r['b_type']} ({r['b_conc']}) | "
                  f"{r['b_rt']:.1f} | {r['d_rt']:+.1f} | {pct} | {r['rchar']} | {r['wchar']} |")
    summary(rows, 'all tasks', lambda r: True)
    summary(rows, 'rt >= 5 s, same type', lambda r: r['same_type'] and r['a_rt'] >= 5)
    summary(rows, 'rt >= 5 s, same type, same conc',
            lambda r: r['same_type'] and r['a_rt'] >= 5 and r['a_conc'] == r['b_conc'])


if __name__ == '__main__':
    main()
