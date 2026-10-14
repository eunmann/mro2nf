#!/usr/bin/env python3
"""Join the Nextflow trace with the Batch job records: each task's queue, in-container time, non-compute and poll lag.

  queue  = Batch createdAt -> startedAt (scheduling, instance launch, image pull)
  incont = Batch startedAt -> stoppedAt
  nc     = incont - trace realtime (staging and the task wrapper)
  poll   = trace end -> Batch stoppedAt; poll_log uses the .nextflow.log "Task completed" time (ms resolution),
           where the trace's duration is whole seconds above a minute

usage: phases.py RUN_DIR   (reads trace.txt, jobs.json, nextflow.log, cimap.json if present; writes tasks.json)
"""
import argparse
import datetime as dt
import os
import re
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import perfrun

COMPLETED = re.compile(r'(\w{3}-\d\d \d\d:\d\d:\d\d\.\d{3}) .*Task completed > TaskHandler\[id: (\d+);')


def completions(nflog, year):
    out = {}
    for line in open(nflog):
        m = COMPLETED.match(line)
        if m:
            out[m.group(2)] = dt.datetime.strptime(f'{year}-{m.group(1)}', '%Y-%b-%d %H:%M:%S.%f')
    return out


def local(ms):
    return dt.datetime.fromtimestamp(ms / 1000)


def join(r, j, cimap, logend):
    container = j.get('container') or {}
    started, stopped = local(j['startedAt']), local(j['stoppedAt'])
    r['created'], r['started'], r['stopped'] = local(j['createdAt']), started, stopped
    r['queue'] = (started - r['created']).total_seconds()
    r['incont'] = (stopped - started).total_seconds()
    r['nc'] = r['incont'] - (r['rt'] or 0)
    r['poll'] = (r['end'] - stopped).total_seconds()
    r['natt'] = len(j.get('attempts') or [])
    ci = perfrun.instance_id(container.get('containerInstanceArn'))
    r['inst'] = ci[:8]
    r['ls'] = container.get('logStreamName', '')
    r['vcpus'] = next((x['value'] for x in container.get('resourceRequirements', []) if x['type'] == 'VCPU'), None)
    r['itype'] = cimap.get(ci, {}).get('type')
    r['ec2'] = cimap.get(ci, {}).get('ec2')
    r['end_log'] = logend.get(r['task_id'])
    r['poll_log'] = (r['end_log'] - stopped).total_seconds() if r['end_log'] else None
    return r


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument('run_dir')
    a = ap.parse_args()
    rows = perfrun.load_trace(perfrun.path(a.run_dir, 'trace.txt'))
    jl = perfrun.read_json(a.run_dir, 'jobs.json')
    jobs = {j['jobId']: j for j in (jl['jobs'] if isinstance(jl, dict) else jl)}
    t0 = min(r['sub'] for r in rows)
    cimap = perfrun.read_json(a.run_dir, 'cimap.json') if os.path.exists(perfrun.path(a.run_dir, 'cimap.json')) else {}
    logend = completions(perfrun.path(a.run_dir, 'nextflow.log'), t0.year)
    out = [join(r, jobs[r['native_id']], cimap, logend) for r in rows]
    perfrun.write_json(a.run_dir, 'tasks.json',
                       [{k: (v.isoformat() if isinstance(v, dt.datetime) else v) for k, v in r.items()} for r in out],
                       indent=1)
    for r in sorted(out, key=lambda r: r['sub']):
        print(f"{r['task_id']:>4} {(r['sub']-t0).total_seconds():6.0f}-{(r['end']-t0).total_seconds():6.0f} "
              f"q={r['queue']:6.1f} in={r['incont']:6.1f} rt={r['rt'] or 0:6.1f} nc={r['nc']:5.1f} poll={r['poll']:5.1f} "
              f"cpu={r['vcpus']} inst={r['inst']} att={r['natt']} {r['short']}")


if __name__ == '__main__':
    main()
