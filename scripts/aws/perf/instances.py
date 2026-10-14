#!/usr/bin/env python3
"""Resolve each container instance that ran the run's jobs to its EC2 instance and type, and fetch the types' specs.

Sources, in order: `ecs describe-container-instances`, `ec2 describe-instances`, and cimap.json (CloudTrail).
ECS and EC2 forget terminated instances within hours; CloudTrail keeps them for 90 days, and its registration
time tells a cold start (the instance registered after the job was created) from a warm one.
Calls the AWS CLI read-only, in its configured region.

usage: instances.py RUN_DIR   (reads jobs.json, cimap.json; writes instances.json and instance_types.json)
"""
import argparse
import json
import os
import subprocess
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import perfrun


def aws(*args):
    r = subprocess.run(['aws', *args, '--output', 'json'], capture_output=True, text=True)
    return json.loads(r.stdout) if r.returncode == 0 and r.stdout.strip() else {}


def ecs_records(arns):
    by = {}
    for i in range(0, len(arns), 100):
        part = arns[i:i + 100]
        ecs = aws('ecs', 'describe-container-instances', '--cluster', part[0].split('/')[1], '--container-instances', *part)
        by.update({perfrun.instance_id(c['containerInstanceArn']): c for c in ecs.get('containerInstances', [])})
    return by


def ec2_records(ids):
    if not ids:
        return {}
    ec2 = aws('ec2', 'describe-instances', '--instance-ids', *ids)
    if not ec2:
        # One purged ID fails the whole call; ask for one at a time.
        ec2 = {'Reservations': [x for i in ids for x in aws('ec2', 'describe-instances', '--instance-ids', i)
                                .get('Reservations', [])]}
    return {i['InstanceId']: i for rv in ec2.get('Reservations', []) for i in rv['Instances']}


def resolve(arns, ct):
    ecs_by = ecs_records(arns)
    res = {}
    for a in arns:
        k = perfrun.instance_id(a)
        e, c = ecs_by.get(k), ct.get(k, {})
        res[k] = dict(ecs_status=e['status'] if e else 'MISSING', ec2=(e or {}).get('ec2InstanceId') or c.get('ec2'),
                      ecs_type=next((x['value'] for x in (e or {}).get('attributes', [])
                                     if x['name'] == 'ecs.instance-type'), None),
                      ct_type=c.get('type'), ct_registered=c.get('registered'), ct_pending=c.get('pending'))
    ec2_by = ec2_records(sorted({r['ec2'] for r in res.values() if r['ec2']}))
    for r in res.values():
        i = ec2_by.get(r['ec2'])
        r['ec2_type'] = i['InstanceType'] if i else None
        r['ec2_state'] = i['State']['Name'] if i else 'NOT FOUND'
        r['ec2_launch'] = i['LaunchTime'] if i else None
        r['type'] = r['ecs_type'] or r['ec2_type'] or r['ct_type']
        r['consistent'] = len({t for t in (r['ecs_type'], r['ec2_type'], r['ct_type']) if t}) == 1
    return res


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument('run_dir')
    a = ap.parse_args()
    jobs = perfrun.read_json(a.run_dir, 'jobs.json')
    arns = sorted({x['container']['containerInstanceArn'] for j in jobs for x in j.get('attempts', [])
                   if x.get('container', {}).get('containerInstanceArn')})
    res = resolve(arns, perfrun.read_json(a.run_dir, 'cimap.json'))
    perfrun.write_json(a.run_dir, 'instances.json', res, indent=1)
    types = sorted({r['type'] for r in res.values() if r['type']})
    perfrun.write_json(a.run_dir, 'instance_types.json',
                       aws('ec2', 'describe-instance-types', '--instance-types', *types) if types else {})
    print(f"{len(res)} container instances; ECS found {sum(r['ecs_status'] != 'MISSING' for r in res.values())}, "
          f"EC2 found {sum(r['ec2_state'] != 'NOT FOUND' for r in res.values())}, "
          f"CloudTrail {sum(bool(r['ct_type']) for r in res.values())}, "
          f"all sources consistent {sum(r['consistent'] for r in res.values())}")
    for k, r in sorted(res.items(), key=lambda kv: kv[1]['ct_registered'] or ''):
        print(f"  {k[:8]} {r['ec2']} {r['type'] or '-':12s} ecs={r['ecs_status']:8s} ec2={r['ec2_state']:10s} "
              f"registered={r['ct_registered']}")


if __name__ == '__main__':
    main()
