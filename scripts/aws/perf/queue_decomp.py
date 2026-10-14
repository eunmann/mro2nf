#!/usr/bin/env python3
"""Split each task's Batch queue time (createdAt -> startedAt) using its ECS task and instance records.

  sched  = Batch createdAt -> ECS task createdAt (Batch placing the job; for a cold start this includes the wait
           for the instance: the scale-up decision, EC2 launch, boot, ECS registration)
  launch = Batch createdAt -> EC2 pendingTime (cold starts only: when the instance that ran it was requested)
  boot   = EC2 pendingTime -> ECS registration (cold starts only)
  pull   = ECS pullStartedAt -> pullStoppedAt
  start  = ECS createdAt -> Batch startedAt, less the pull
A cold start is a task whose instance registered after its job was created.

usage: queue_decomp.py RUN_DIR   (reads tasks.json, jobs.json, ecs_tasks.json, instances.json, cp_log.json;
                                  writes queue_decomp.csv; markdown on stdout)
"""
import argparse
import os
import statistics as st
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import perfrun

P = perfrun.iso


def decompose(t, job, ecs, inst, on_cp):
    a = job['attempts'][-1]['container']
    e = ecs[a['taskArn']]
    i = inst[perfrun.instance_id(a['containerInstanceArn'])]
    cr, started = P(t['created']), P(t['started'])
    reg, pend = P(i['ct_registered']), P(i['ct_pending'])
    cold = reg > cr
    pull = (P(e['pullStoppedAt']) - P(e['pullStartedAt'])).total_seconds() if e.get('pullStartedAt') else 0.0
    return dict(id=t['task_id'], key=t['short'], on_cp=on_cp, queue=round(t['queue'], 1), cold=cold, itype=i['type'],
                launch=round((pend - cr).total_seconds(), 1) if cold else None,
                boot=round((reg - pend).total_seconds(), 1) if cold else None,
                sched=round((P(e['createdAt']) - cr).total_seconds(), 1), pull=round(pull, 1),
                start=round((started - P(e['createdAt'])).total_seconds() - pull, 1))


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument('run_dir')
    a = ap.parse_args()
    jobs = {j['jobId']: j for j in perfrun.read_json(a.run_dir, 'jobs.json')}
    ecs = {e['taskArn']: e for e in perfrun.read_json(a.run_dir, 'ecs_tasks.json')}
    inst = perfrun.read_json(a.run_dir, 'instances.json')
    on_path = {x['id'] for x in perfrun.read_json(a.run_dir, 'cp_log.json')}
    rows = [decompose(t, jobs[t['native_id']], ecs, inst, tid in on_path)
            for tid, t in perfrun.tasks_by_id(a.run_dir).items()]
    perfrun.write_csv(perfrun.path(a.run_dir, 'queue_decomp.csv'), rows)
    print('| id | task | queue | cold | type | launch (created->EC2 pending) | boot (pending->registered) | '
          'sched (->ECS task) | pull | start |')
    print('|---|---|---:|---|---|---:|---:|---:|---:|---:|')
    for r in rows:
        if r['on_cp'] and (r['cold'] or r['queue'] > 15):
            print(f"| {r['id']} | {r['key']} | {r['queue']} | {r['cold']} | {r['itype']} | {r['launch']} | {r['boot']} | "
                  f"{r['sched']} | {r['pull']} | {r['start']} |")
    for lbl, sel in (('CP warm', lambda r: r['on_cp'] and not r['cold']), ('all warm', lambda r: not r['cold']),
                     ('CP cold', lambda r: r['on_cp'] and r['cold'])):
        v = [r for r in rows if sel(r)]
        if v:
            print(f"{lbl}: n={len(v)} queue sum {sum(r['queue'] for r in v):.0f} median {st.median(r['queue'] for r in v):.1f} "
                  f"sched median {st.median(r['sched'] for r in v):.1f} pull median {st.median(r['pull'] for r in v):.1f} "
                  f"pull max {max(r['pull'] for r in v):.1f} start median {st.median(r['start'] for r in v):.1f}")


if __name__ == '__main__':
    main()
