#!/usr/bin/env python3
"""Cross-run analysis of AWS Batch runs: critical path, poll lag, launch to PLAN, a run-pair staging comparison,
the theoretical and practical floors, and where the gap between wall time and the practical floor goes.

Each RUN_DIR has been through phases.py, stagein.py, cp.py (--end log) and instances.py; its directory name
labels it. A --baseline run (one before the change being measured) appears in the critical-path, poll and
launch tables only: the minima, floors and gap ranking come from the other runs.

  theoretical floor  the longest chain of dependent tasks counting compute (trace realtime) only
  practical floor    the longest chain counting each task's compute, the wrapper floor, the poll floor, the
                     least dispatch and warm queue seen, and the bytes it must stage at the best staging rate
                     seen, plus the run's PLAN cold start
  gap                the measured critical path's components above those floors

--requeue TARGET=SOURCE:PROCESS backs an external queue block out of TARGET: its PROCESS tasks take SOURCE's
queue times for the same tasks, the path shifts accordingly, and TARGET is reported again as TARGETadj.

usage: analyze.py [-o OUT] [--baseline LABEL] [--requeue T=S:PROC] [--pair A,B] [--watch PROC,...]
                  [--volume-mibps LABEL=N ...] RUN_DIR...
writes OUT/cp_summary.csv, pair_heavy.csv, pair_by_itype.csv, floors.json, gap_detail.csv, nc_residual.csv,
analysis.json; markdown on stdout
"""
import argparse
import datetime as dt
import json
import os
import statistics as st
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import perfrun

HEAVY = 1e9
COMP = ['dispatch', 'queue', 'nc', 'rt', 'poll']
P = dt.datetime.fromisoformat


def parse_args():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument('-o', '--out', default='.')
    ap.add_argument('--baseline', action='append', default=[], help='a run before the change: tables only')
    ap.add_argument('--requeue', help='TARGET=SOURCE:PROCESS')
    ap.add_argument('--pair', help='A,B: compare the heavy stagers of run A and run B')
    ap.add_argument('--watch', default='', help='processes, comma-separated, to add to the --pair table')
    ap.add_argument('--volume-mibps', action='append', default=[], help="LABEL=N: the run's root-volume MiB/s")
    ap.add_argument('--wrap-floor', type=float, default=5.5, help="s: the least nc of a task that stages nothing")
    ap.add_argument('--poll-floor', type=float, default=1.0, help='s: the mean wait on the poll interval')
    ap.add_argument('run_dirs', nargs='+')
    return ap.parse_args()


def load(run_dir):
    tasks = perfrun.tasks_by_id(run_dir)
    si = perfrun.read_json(run_dir, 'stagein.json')
    for k, t in perfrun.keyed(tasks.values()).items():
        t['key'] = k
    for tid, t in tasks.items():
        t['bin'] = si[tid].get('bytes_in')
        t['bout'] = si[tid].get('bytes_out')
        t['rchar_b'] = perfrun.size(t['rchar'])
        t['prods'] = [p for p in si[tid]['producers'] if p != '?']
    concurrency(tasks)

    def optional(name):
        return perfrun.read_json(run_dir, name) if os.path.exists(perfrun.path(run_dir, name)) else {}

    return dict(dir=run_dir, tasks=tasks, si=si, cp=perfrun.read_json(run_dir, 'cp_log.json'),
                jobs={j['jobId']: j for j in perfrun.read_json(run_dir, 'jobs.json')},
                insts=optional('instances.json'), itypes=optional('instance_types.json'))


def concurrency(tasks):
    for t in tasks.values():
        a, b = P(t['started']), P(t['stopped'])
        same = [x for x in tasks.values() if x is not t and x['inst'] == t['inst']
                and P(x['started']) < b and P(x['stopped']) > a]
        t['conc'] = len(same)
        t['conc_bytes'] = sum((x['bin'] or 0) + (x['bout'] or 0) for x in same)


def t0(tasks):
    return min(P(t['sub']) for t in tasks.values())


def dag(tasks):
    """File-producer edges, PLAN before every task, and cp.py's value-dependency rule applied to every task."""
    plan = next(k for k, t in tasks.items() if t['short'] == 'PLAN')
    preds = {}
    for k, t in tasks.items():
        ps = set(t['prods'])
        if k != plan:
            ps.add(plan)
            last = max((P(tasks[p]['end_log']) for p in ps), default=None)
            if last is None or (P(t['sub']) - last).total_seconds() > 5:
                v = max((x for x in tasks.values() if P(x['end_log']) <= P(t['sub'])), key=lambda x: P(x['end_log']))
                ps.add(v['task_id'])
        preds[k] = ps
    return preds


def longest(tasks, preds, w):
    best, back = {}, {}
    for k in sorted(tasks, key=lambda k: P(tasks[k]['sub'])):
        b = max(((best[p], p) for p in preds[k] if p in best), default=(0.0, None))
        best[k], back[k] = b[0] + w(tasks[k]), b[1]
    end = max(best, key=best.get)
    chain = []
    while end:
        chain.append(end)
        end = back[end]
    return best[chain[0]], chain[::-1]


def cp_totals(rows):
    tot = {k: sum(x[k] for x in rows) for k in COMP}
    return dict(wall=sum(tot.values()), n=len(rows), **tot)


def descendants(preds, roots):
    desc, grow = set(roots), True
    while grow:
        grow = False
        for k, ps in preds.items():
            if k not in desc and ps & desc:
                desc.add(k)
                grow = True
    return desc


def requeue(target, source, proc):
    """TARGET's critical path with PROCESS's queue times replaced by SOURCE's, and the shift that implies."""
    tt, cpt = target['tasks'], target['cp']
    qs = {t['key']: t['queue'] for t in source['tasks'].values() if t['short'] == proc}
    mn = [t for t in tt.values() if t['short'] == proc]
    endp = {t['task_id']: P(t['end_log']) - dt.timedelta(seconds=t['queue'] - qs[t['key']]) for t in mn}
    new_k = max(endp, key=endp.get)
    delta = (endp[new_k] - max(P(t['end_log']) for t in mn)).total_seconds()
    i = next(j for j, x in enumerate(cpt) if x['name'] == proc)
    c = tt[new_k]
    row = dict(cpt[i], id=new_k, dispatch=(P(c['created']) - P(tt[cpt[i - 1]['id']]['end_log'])).total_seconds(),
               queue=qs[c['key']], nc=c['nc'], rt=c['rt'] or 0, poll=c['poll_log'])
    nxt = dict(cpt[i + 1], dispatch=(P(tt[cpt[i + 1]['id']]['created']) + dt.timedelta(seconds=delta)
                                     - endp[new_k]).total_seconds())
    preds = dag(tt)
    desc = descendants(preds, endp)
    # The shift is valid while no task outside the requeued tasks' descendants finishes after a descendant it
    # feeds is submitted: the least such slack is reported.
    slack = min(((P(tt[y]['sub']) + dt.timedelta(seconds=delta) - P(tt[x]['end_log'])).total_seconds(), x, y)
                for y in desc - set(endp) for x in preds[y] - desc)
    return (cpt[:i] + [row, nxt] + cpt[i + 2:], delta, slack,
            {t['key']: (round(t['queue'], 1), round(qs[t['key']], 1)) for t in mn})


def section_cp(a, D, out):
    cpt = {r: cp_totals(d['cp']) for r, d in D.items()}
    paths = {r: (r, d['cp']) for r, d in D.items()}
    if a.requeue:
        tgt, rest = a.requeue.split('=')
        src, proc = rest.split(':', 1)
        rows, delta, slack, qrep = requeue(D[tgt], D[src], proc)
        cpt[f'{tgt}adj'] = cp_totals(rows)
        paths[f'{tgt}adj'] = (tgt, rows)
        out['requeue'] = dict(target=tgt, delta_s=delta, min_slack_s=slack[0], slack_pair=slack[1:], queue_replaced=qrep)
        perfrun.write_json(D[tgt]['dir'], 'cp_log_adjusted.json', rows, indent=1)
    print('## 1. Critical path\n')
    print('| run | wall s | tasks | ' + ' | '.join(f'{c} s (%)' for c in COMP) + ' |')
    print('|---|---:|---:|' + '---:|' * len(COMP))
    for r, v in cpt.items():
        print(f"| {r} | {v['wall']:.0f} | {v['n']} | " + ' | '.join(f"{v[c]:.0f} ({100*v[c]/v['wall']:.1f}%)" for c in COMP) + ' |')
    perfrun.write_csv(os.path.join(a.out, 'cp_summary.csv'), [
        dict(run=r, wall_s=round(v['wall'], 1), tasks=v['n'], **{f'{c}_s': round(v[c], 1) for c in COMP},
             **{f'{c}_pct': round(100 * v[c] / v['wall'], 2) for c in COMP}) for r, v in cpt.items()])
    if a.requeue:
        q = out['requeue']
        print(f"\nrun {q['target']} requeued: delta {q['delta_s']:.0f} s; queue (run, source) per task {q['queue_replaced']}; "
              f"min slack of a non-descendant feeding a shifted task {q['min_slack_s']:.0f} s "
              f"(task {q['slack_pair'][0]} -> {q['slack_pair'][1]})")
    return cpt, paths


def launch_marks(nflog, year):
    def stamp(line):
        return dt.datetime.strptime(f'{year}-' + line[:18], '%Y-%b-%d %H:%M:%S.%f')

    lines = open(nflog).read().split('\n')
    marks = {'launch': stamp(lines[0])}
    for line in lines:
        for m, s in (('session', 'Session start'), ('exec', '> Launching execution'), ('plan', 'Submitted process > PLAN')):
            if s in line and m not in marks:
                marks[m] = stamp(line)
    return marks


def section_poll_launch(D, cpt, out):
    print('\n### Poll lag\n')
    print('| run | CP poll s | CP mean/task | all-task mean | all-task median | all-task max |')
    print('|---|---:|---:|---:|---:|---:|')
    for r, d in D.items():
        pl = [t['poll_log'] for t in d['tasks'].values()]
        print(f"| {r} | {cpt[r]['poll']:.0f} | {cpt[r]['poll']/cpt[r]['n']:.2f} | {st.mean(pl):.2f} | "
              f"{st.median(pl):.2f} | {max(pl):.2f} |")
    print('\n### Nextflow launch -> PLAN\n')
    print('| run | launch | ->Session start | ->Launching execution | ->PLAN submitted | PLAN queue (cold start) | launch->OUTS done |')
    print('|---|---|---:|---:|---:|---:|---:|')
    out['launch_to_plan'] = {}
    for r, d in D.items():
        marks = launch_marks(perfrun.path(d['dir'], 'nextflow.log'), t0(d['tasks']).year)
        plan = next(t for t in d['tasks'].values() if t['short'] == 'PLAN')
        lp = {k: (v - marks['launch']).total_seconds() for k, v in marks.items()}
        lp['plan_queue'] = plan['queue']
        out['launch_to_plan'][r] = lp
        print(f"| {r} | {marks['launch'].strftime('%m-%d %H:%M:%S')} | {lp['session']:.1f} | {lp['exec']:.1f} | "
              f"{lp['plan']:.1f} | {plan['queue']:.1f} | {lp['plan'] + cpt[r]['wall']:.0f} |")


def moved(t):
    return (t['bin'] or 0) + (t['bout'] or 0)


def need(t):
    """The bytes a task must move: what it stages that it reads, and what it writes."""
    return min(t['bin'] or 0, t['rchar_b']) + (t['bout'] or 0)


def rate(t, wrap):
    """A task's effective staging MB/s: the bytes it moved over its nc above the wrapper floor."""
    return moved(t) / 1e6 / max(t['nc'] - wrap, 0.1)


def best_rate(D, late, wrap, out):
    cands = [(rate(t, wrap), r, t['key'], t['itype']) for r in late for t in D[r]['tasks'].values() if moved(t) >= HEAVY]
    if not cands:
        sys.exit(f'analyze: no task of runs {late} moved 1 GB or more, so no best staging rate is measured')
    best = max(cands)
    out['best_MBps'] = dict(value=best[0], run=best[1], task=best[2], itype=best[3])
    return best


def fmt(v, spec='.0f'):
    return '-' if v is None else format(v, spec)


def section_pair(a, D, ebs, vol, best):
    A, B = a.pair.split(',')
    kA = {t['key']: t for t in D[A]['tasks'].values()}
    kB = {t['key']: t for t in D[B]['tasks'].values()}
    watch = set(filter(None, a.watch.split(',')))
    keys = sorted((k for k in kA.keys() & kB.keys() if kA[k]['short'] in watch or max(moved(kA[k]), moved(kB[k])) > HEAVY),
                  key=lambda k: P(kA[k]['sub']))
    rows = []
    for k in keys:
        x, y = kA[k], kB[k]
        rows.append(dict(key=k, **{f'{p}_{f}': t[f] for p, t in (('ra', x), ('rb', y)) for f in ('itype', 'conc', 'nc', 'rt', 'queue')},
                         MB_in=round((x['bin'] or 0) / 1e6), MB_out=round((x['bout'] or 0) / 1e6),
                         ra_MBps=round(rate(x, a.wrap_floor)), rb_MBps=round(rate(y, a.wrap_floor)),
                         ra_ebs=ebs.get(x['itype']), rb_ebs=ebs.get(y['itype'])))
    if rows:
        perfrun.write_csv(os.path.join(a.out, 'pair_heavy.csv'), rows)
    print(f'\n## 2. Heavy stagers, run {A} -> run {B} (ra = run {A}, rb = run {B})\n')
    print(f'| task | MB in | MB out | r{A} type (conc) | r{A} nc | r{A} MB/s | r{B} type (conc) | r{B} nc | r{B} MB/s | d nc | r{A} rt | r{B} rt |')
    print('|---|---:|---:|---|---:|---:|---|---:|---:|---:|---:|---:|')
    for x in rows:
        print(f"| {x['key']} | {x['MB_in']} | {x['MB_out']} | {x['ra_itype']} ({x['ra_conc']}) | {x['ra_nc']:.1f} | {x['ra_MBps']} | "
              f"{x['rb_itype']} ({x['rb_conc']}) | {x['rb_nc']:.1f} | {x['rb_MBps']} | {x['rb_nc']-x['ra_nc']:+.1f} | "
              f"{x['ra_rt'] or 0:.1f} | {x['rb_rt'] or 0:.1f} |")
    print(f"\nsum nc over these {len(rows)} tasks: run {A} {sum(x['ra_nc'] for x in rows):.0f} s, "
          f"run {B} {sum(x['rb_nc'] for x in rows):.0f} s ({sum(x['rb_nc'] - x['ra_nc'] for x in rows):+.0f} s)")
    print(f'\n### Effective staging MB/s by instance type (tasks moving >= 100 MB; MB/s = (in+out)/(nc-{a.wrap_floor}))\n')
    print('| run | type | n | median MB/s | max MB/s | EBS baseline MB/s | root-volume cap MB/s |')
    print('|---|---|---:|---:|---:|---:|---:|')
    bt = []
    for r in (A, B):
        g = {}
        for t in D[r]['tasks'].values():
            if moved(t) >= 1e8:
                g.setdefault(t['itype'], []).append(rate(t, a.wrap_floor))
        for it, v in sorted(g.items(), key=lambda kv: kv[0] or ''):
            bt.append(dict(run=r, itype=it, n=len(v), median=round(st.median(v)), max=round(max(v)),
                           ebs_baseline=ebs.get(it), volume_cap=round(vol[r]) if r in vol else None))
            print(f"| {r} | {it} | {len(v)} | {st.median(v):.0f} | {max(v):.0f} | {fmt(ebs.get(it), '')} | {fmt(vol.get(r))} |")
    if bt:
        perfrun.write_csv(os.path.join(a.out, 'pair_by_itype.csv'), bt)
    print(f"\nbest effective MB/s over tasks moving >= 1 GB (runs {[r for r in D if r not in a.baseline]}): "
          f"{best[0]:.0f} ({best[1:]})")


def floor_of(a, d, m, best):
    """The run's theoretical and practical floors; m holds the minima across runs."""
    tasks, cp = d['tasks'], d['cp']
    preds = dag(tasks)
    th_chain, ch = longest(tasks, preds, lambda t: t['rt'] or 0)
    plan_q = next(t for t in tasks.values() if t['short'] == 'PLAN')['queue']

    def fw(t):
        return (t['rt'] or 0) + a.wrap_floor + m['dmin'] + a.poll_floor + m['qmin'] + need(t) / 1e6 / best

    pr_chain, pch = longest(tasks, preds, fw)
    return dict(theo_cp_rt=sum(x['rt'] for x in cp), theo_longest_rt_chain=th_chain,
                theo_chain=[tasks[k]['key'] for k in ch], practical=pr_chain - m['qmin'] + plan_q,
                practical_on_measured_cp=sum(fw(tasks[x['id']]) for x in cp) - m['qmin'] + plan_q,
                practical_chain=[tasks[k]['key'] for k in pch], plan_cold_start=plan_q,
                staging_floor_s=sum(need(tasks[k]) / 1e6 / best for k in pch))


def section_floors(a, D, late, cpt, paths, best, out):
    m = dict(qmin=min(t['queue'] for r in late for t in D[r]['tasks'].values()),
             dmin=min(x['dispatch'] for r in late for x in D[r]['cp'] if x['name'] != 'PLAN'))
    out.update(qmin=m['qmin'], dmin=m['dmin'], F_wrap=a.wrap_floor, poll_floor=a.poll_floor)
    print(f"\n## 3. Floors\n\nwarm queue min (runs {late}, all tasks) {m['qmin']:.2f} s; dispatch min (CP, runs {late}) "
          f"{m['dmin']:.2f} s; wrapper floor {a.wrap_floor} s; poll {a.poll_floor} s; best staging {round(best)} MB/s\n")
    floors = {}
    for r in late:
        floors[r] = dict(wall=cpt[r]['wall'], **floor_of(a, D[r], m, best))
        if f'{r}adj' in cpt:
            floors[f'{r}adj'] = dict(floors[r], wall=cpt[f'{r}adj']['wall'])
    perfrun.write_json(a.out, 'floors.json', floors, indent=1)
    print('| run | wall s | theoretical: CP rt sum | theoretical: longest rt chain | % of wall | practical floor | % of wall | wall - practical |')
    print('|---|---:|---:|---:|---:|---:|---:|---:|')
    for r, f in floors.items():
        th = max(f['theo_cp_rt'], f['theo_longest_rt_chain'])
        print(f"| {r} | {f['wall']:.0f} | {f['theo_cp_rt']:.0f} | {f['theo_longest_rt_chain']:.0f} | {100*th/f['wall']:.1f}% | "
              f"{f['practical']:.0f} | {100*f['practical']/f['wall']:.1f}% | {f['wall']-f['practical']:.0f} |")
    for r, f in floors.items():
        run, rows = paths[r]
        cpk = [D[run]['tasks'][x['id']]['key'] for x in rows]
        print(f"\nrun {r}: longest rt chain differs from the measured CP at: {[k for k in f['theo_chain'] if k not in cpk]}")
        print(f"run {r}: practical floor = {f['practical']:.0f} s (on measured CP {f['practical_on_measured_cp']:.0f}); "
              f"PLAN cold start {f['plan_cold_start']:.0f} s; staging-at-best on floor chain {f['staging_floor_s']:.0f} s; "
              f"chain {len(f['practical_chain'])} tasks")
    return m


GAPS = ['queue: PLAN cold start', 'queue: mid-run instance cold starts', 'queue: warm, above min',
        'nc: staging bytes the stage does not read', 'nc: staging needed bytes below best MB/s',
        'nc: unexplained residual', 'poll above 1 s', 'dispatch above min', 'compute (rt)']


def cold_start(d, t):
    """Whether the task's instance registered after its job was created."""
    att = d['jobs'][t['native_id']].get('attempts', [])[-1:]
    ci = perfrun.instance_id(att[0]['container']['containerInstanceArn']) if att else ''
    reg = d['insts'].get(ci, {}).get('ct_registered')
    return bool(reg) and perfrun.iso(reg) > P(t['created'])


def split_nc(t, nc_x, best):
    """nc above the wrapper floor as (unread staging, needed staging at the best rate, needed staging below it).

    A task moving 100 MB or more is dominated by staging, so its nc is apportioned by bytes; for a smaller one,
    only its staging at the best rate is charged, and the rest is left to the unexplained residual.
    """
    B = moved(t)
    unread = max(0, (t['bin'] or 0) - t['rchar_b'])
    stg_need = need(t) / 1e6 / best
    if B >= 1e8:
        unread_s = nc_x * unread / B
        stg_need = min(stg_need, nc_x - unread_s)
        return unread_s, stg_need, nc_x - unread_s - stg_need
    unread_s = min(nc_x, unread / 1e6 / best)
    return unread_s, min(stg_need, nc_x - unread_s), 0.0


def gap_rows(a, d, r, rows, m, best):
    tasks = d['tasks']
    g = dict.fromkeys(GAPS, 0.0)
    detail = []
    for x in rows:
        t = tasks[x['id']]
        q = x['queue'] - m['qmin']
        cold = cold_start(d, t)
        g['queue: PLAN cold start' if t['short'] == 'PLAN' else
          'queue: mid-run instance cold starts' if cold else 'queue: warm, above min'] += q
        nc_x = max(0.0, x['nc'] - a.wrap_floor)
        unread_s, stg_need, slow = split_nc(t, nc_x, best)
        resid = nc_x - unread_s - stg_need - slow
        g['nc: staging bytes the stage does not read'] += unread_s
        g['nc: staging needed bytes below best MB/s'] += slow
        g['nc: unexplained residual'] += resid
        g['poll above 1 s'] += x['poll'] - a.poll_floor
        g['dispatch above min'] += x['dispatch'] - m['dmin']
        g['compute (rt)'] += x['rt']
        detail.append(dict(run=r, id=x['id'], key=t['key'], itype=t['itype'], conc=t['conc'], queue=round(x['queue'], 1),
                           cold=cold, nc=round(x['nc'], 1), MB_in=round((t['bin'] or 0) / 1e6),
                           MB_out=round((t['bout'] or 0) / 1e6), MB_read=round(t['rchar_b'] / 1e6),
                           unread_s=round(unread_s, 1), slow_s=round(slow, 1), resid_s=round(resid, 1), rt=x['rt'],
                           poll=round(x['poll'], 1), dispatch=round(x['dispatch'], 1)))
    return g, detail


def section_gap(a, D, late, cpt, paths, m, best, out):
    print('\n## 4. Where the gap goes\n')
    alld = []
    for r in late:
        lbl = f'{r}adj' if f'{r}adj' in cpt else r
        g, detail = gap_rows(a, D[r], r, paths[lbl][1], m, best)
        alld += detail
        out[f'gap_{r}'] = g
        wall = cpt[lbl]['wall']
        print(f"### run {lbl}: wall {wall:.0f} s\n")
        print('| component | s | % of wall |')
        print('|---|---:|---:|')
        for k, v in sorted(g.items(), key=lambda kv: -kv[1]):
            print(f"| {k} | {v:.0f} | {100*v/wall:.1f}% |")
        n, per = len(paths[lbl][1]), a.wrap_floor + a.poll_floor + m['dmin'] + m['qmin']
        print(f"| floors on path: {n} x (wrapper {a.wrap_floor} + poll {a.poll_floor} + dispatch {m['dmin']:.1f} + "
              f"queue {m['qmin']:.1f}) | {n*per:.0f} | {100*n*per/wall:.1f}% |\n")
    perfrun.write_csv(os.path.join(a.out, 'gap_detail.csv'), alld)


def section_residual(a, D, late, best):
    print(f'### Tasks with nc far above floor without a staging explanation '
          f'(resid = nc - {a.wrap_floor} - (in+out)/best > 8 s)\n')
    print('| run | task | type | co-running tasks | their MB in+out | nc | own MB in+out | staging@best s | resid s |')
    print('|---|---|---|---:|---:|---:|---:|---:|---:|')
    res = []
    for r in late:
        for t in D[r]['tasks'].values():
            sb = moved(t) / 1e6 / best
            res.append(dict(run=r, key=t['key'], itype=t['itype'], conc=t['conc'], conc_MB=round(t['conc_bytes'] / 1e6),
                            nc=round(t['nc'], 1), MB=round(moved(t) / 1e6), stage_best_s=round(sb, 1),
                            resid=round(t['nc'] - a.wrap_floor - sb, 1)))
    res.sort(key=lambda x: -x['resid'])
    for x in res:
        if x['resid'] > 8:
            print(f"| {x['run']} | {x['key']} | {x['itype']} | {x['conc']} | {x['conc_MB']} | {x['nc']} | {x['MB']} | "
                  f"{x['stage_best_s']} | {x['resid']} |")
    perfrun.write_csv(os.path.join(a.out, 'nc_residual.csv'), res)


def main():
    a = parse_args()
    os.makedirs(a.out, exist_ok=True)
    D = {perfrun.label(d): load(d) for d in a.run_dirs}
    late = [r for r in D if r not in a.baseline]
    vol = {k: float(v) * 1.048576 for k, v in (x.split('=') for x in a.volume_mibps)}
    ebs = {i['InstanceType']: i.get('EbsInfo', {}).get('EbsOptimizedInfo', {}).get('BaselineThroughputInMBps')
           for d in D.values() for i in d['itypes'].get('InstanceTypes', [])}
    out = {}
    cpt, paths = section_cp(a, D, out)
    section_poll_launch(D, cpt, out)
    best = best_rate(D, late, a.wrap_floor, out)
    if a.pair:
        section_pair(a, D, ebs, vol, best)
    m = section_floors(a, D, late, cpt, paths, best[0], out)
    section_gap(a, D, late, cpt, paths, m, best[0], out)
    section_residual(a, D, late, best[0])
    perfrun.write_json(a.out, 'analysis.json', out, indent=1, default=str)


if __name__ == '__main__':
    main()
