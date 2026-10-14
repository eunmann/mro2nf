#!/usr/bin/env python3
"""Per-volume, per-minute EBS load of each run's root volumes against its provisioned gp3 limits, with the tasks
running on the volume's instance in each minute and the phase each was in.

Rates:  avg      = minute sum / 60
        active   = minute sum / (60 - VolumeIdleTime): the rate while the volume had I/O in flight, the in-burst
                   rate a sub-minute burst reached, which a 1-minute average hides
Phases per task (S3 object times, +-1 s; see ncsplit.py): stage = Batch start -> script start, run = the script,
unstage = the first output object -> Batch stop; 'task' where the work dir holds no output object.

usage: ebs_report.py [-o OUT] --gp3 LABEL=MIBPS,IOPS ... RUN_DIR...
       (reads ebs_raw.json, tasks.json, s3ls.txt; writes OUT/ebs_minutes.csv, OUT/ebs_volumes.csv; markdown on stdout)
"""
import argparse
import datetime as dt
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import perfrun

MiB = 2 ** 20
P = perfrun.iso


def phases(run_dir):
    """task ID -> (key, instance, [(phase, start, end)]) from Batch times and the work dir's S3 object times."""
    tasks = perfrun.read_json(run_dir, 'tasks.json')
    root = perfrun.work_dir(tasks)
    objs = perfrun.objects_by_task(run_dir, root)
    out = {}
    for t in tasks:
        o = objs.get(perfrun.rel_dir(root, t['workdir']), [])
        outs = [ts for n, ts, _ in o if not n.split('/')[-1].startswith('.') and not n.endswith('/')]
        a, b = P(t['started']), P(t['stopped'])
        if outs:
            se = min(outs)
            ss = se - dt.timedelta(seconds=t['rt'] or 0)
            ph = [('stage', a, ss), ('run', ss, se), ('unstage', se, b)]
        else:
            ph = [('task', a, b)]
        out[t['task_id']] = dict(key=perfrun.task_key(t), inst=t['inst'], ph=ph)
    return out


def minute(run, v, x, m, ts, ph):
    def g(n):
        return m.get(n, {}).get(ts, 0.0)

    t0 = P(ts)
    t1 = t0 + dt.timedelta(minutes=1)
    act = max(60 - g('VolumeIdleTime'), 0.5)
    byt, ops = g('VolumeReadBytes') + g('VolumeWriteBytes'), g('VolumeReadOps') + g('VolumeWriteOps')
    running = []
    for t in ph.values():
        s = [p for p, a, b in t['ph'] if a < t1 and b > t0] if t['inst'] == x['ci'][:8] else []
        if s:
            running.append(f"{t['key']}[{'+'.join(s)}]")
    return dict(run=run, vol=v, ec2=x['ec2'], type=x['type'], minute=t0.strftime('%H:%M'),
                read_MiBps=round(g('VolumeReadBytes') / 60 / MiB, 1), write_MiBps=round(g('VolumeWriteBytes') / 60 / MiB, 1),
                avg_MiBps=round(byt / 60 / MiB, 1), avg_iops=round(ops / 60), active_s=round(act, 1),
                active_MiBps=round(byt / act / MiB), active_iops=round(ops / act), io_KiB=round(byt / ops / 1024) if ops else 0,
                queue=round(g('VolumeQueueLength'), 2), iops_exceeded=int(g('VolumeIOPSExceededCheck')),
                tput_exceeded=int(g('VolumeThroughputExceededCheck')), stalled=int(g('VolumeStalledIOCheck')),
                w_lat_ms=round(g('VolumeAvgWriteLatency'), 2), r_lat_ms=round(g('VolumeAvgReadLatency'), 2),
                tasks=' '.join(running))


def volume(run, v, x, m, rows, limits):
    mbps_lim, iops_lim = limits
    pk = max(rows, key=lambda r: r['active_MiBps'])
    pi = max(rows, key=lambda r: r['active_iops'])
    return dict(run=run, vol=v, ec2=x['ec2'], ci=x['ci'][:8], type=x['type'], minutes=len(rows),
                GB_written=round(sum(m['VolumeWriteBytes'].values()) / 1e9, 1),
                GB_read=round(sum(m['VolumeReadBytes'].values()) / 1e9, 1),
                peak_avg_MiBps=max(r['avg_MiBps'] for r in rows), peak_avg_iops=max(r['avg_iops'] for r in rows),
                peak_active_MiBps=pk['active_MiBps'], at_MiBps=pk['minute'], peak_active_iops=pi['active_iops'],
                at_iops=pi['minute'], pct_MiBps_limit=round(100 * pk['active_MiBps'] / mbps_lim),
                pct_iops_limit=round(100 * pi['active_iops'] / iops_lim),
                iops_exceeded_min=[r['minute'] for r in rows if r['iops_exceeded']],
                tput_exceeded_min=[r['minute'] for r in rows if r['tput_exceeded']],
                stalled_min=[r['minute'] for r in rows if r['stalled']])


def report(minutes, vols):
    print('| run | volume | instance | type | GB written | GB read | peak 1-min MiB/s | peak 1-min IOPS | '
          'peak active MiB/s (at) | % of MiB/s limit | peak active IOPS (at) | % of IOPS limit | '
          'IOPS-exceeded minutes | tput-exceeded minutes |')
    print('|---|---|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---|---|')
    for v in sorted(vols, key=lambda v: (v['run'], v['ci'])):
        print(f"| {v['run']} | {v['vol']} | {v['ec2']} ({v['ci']}) | {v['type']} | {v['GB_written']} | {v['GB_read']} | "
              f"{v['peak_avg_MiBps']} | {v['peak_avg_iops']} | {v['peak_active_MiBps']} ({v['at_MiBps']}) | "
              f"{v['pct_MiBps_limit']}% | {v['peak_active_iops']} ({v['at_iops']}) | {v['pct_iops_limit']}% | "
              f"{','.join(v['iops_exceeded_min']) or '-'} | {','.join(v['tput_exceeded_min']) or '-'} |")
    print('\n### Minutes with avg >= 50 MiB/s or a limit check set\n')
    print('| run | volume | type | minute | R MiB/s | W MiB/s | IOPS | active s | active MiB/s | active IOPS | IO KiB | '
          'queue | IOPS exc | tput exc | w lat ms | tasks [phase] |')
    print('|---|---|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|')
    for r in minutes:
        if r['avg_MiBps'] >= 50 or r['iops_exceeded'] or r['tput_exceeded']:
            print(f"| {r['run']} | {r['vol'][-6:]} | {r['type']} | {r['minute']} | {r['read_MiBps']} | {r['write_MiBps']} | "
                  f"{r['avg_iops']} | {r['active_s']} | {r['active_MiBps']} | {r['active_iops']} | {r['io_KiB']} | "
                  f"{r['queue']} | {r['iops_exceeded']} | {r['tput_exceeded']} | {r['w_lat_ms']} | {r['tasks']} |")


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument('-o', '--out', default='.')
    ap.add_argument('--gp3', action='append', default=[], required=True, help="LABEL=MIBPS,IOPS: the run's root volume")
    ap.add_argument('run_dirs', nargs='+')
    a = ap.parse_args()
    limits = {k: tuple(int(n) for n in v.split(',')) for k, v in (x.split('=') for x in a.gp3)}
    minutes, vols = [], []
    for d in a.run_dirs:
        run = perfrun.label(d)
        if run not in limits:
            ap.error(f'run {run}: no --gp3 {run}=MIBPS,IOPS')
        with open(perfrun.path(d, 'ebs_raw.json')) as f:
            e = json.load(f)
        ph = phases(d)
        for v, x in e['map'].items():
            m = e['raw'][v]
            rows = [minute(run, v, x, m, ts, ph) for ts in sorted(m['VolumeWriteOps'], key=P)]
            minutes += rows
            vols.append(volume(run, v, x, m, rows, limits[run]))
    for name, data in (('ebs_minutes', minutes), ('ebs_volumes', vols)):
        perfrun.write_csv(os.path.join(a.out, f'{name}.csv'), data)
    report(minutes, vols)


if __name__ == '__main__':
    main()
