#!/usr/bin/env python3
"""Map each ECS container instance to its EC2 instance, from CloudTrail RegisterContainerInstance events.

Reads `aws cloudtrail lookup-events` JSON on stdin and writes, per container-instance ID, only the EC2 instance
ID, instance type, EC2 pendingTime and the registration time. The raw events also carry the registering
instance's temporary STS credentials (responseElements), so pipe them in and never write them to disk.
With --jobs, keeps only the container instances that ran the run's jobs.

usage: aws cloudtrail lookup-events ... --output json | ctmap.py [--jobs RUN_DIR/jobs.json] > RUN_DIR/cimap.json
"""
import argparse
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import perfrun


def job_instances(path):
    with open(path) as f:
        jobs = json.load(f)
    jobs = jobs['jobs'] if isinstance(jobs, dict) else jobs
    return {perfrun.instance_id((a.get('container') or {}).get('containerInstanceArn'))
            for j in jobs for a in (j.get('attempts') or []) + [j]} - {''}


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument('--jobs', help="the run's jobs.json: keep only its container instances")
    a = ap.parse_args()
    keep = job_instances(a.jobs) if a.jobs else None
    out = {}
    for ev in json.load(sys.stdin)['Events']:
        e = json.loads(ev['CloudTrailEvent'])
        arn = ((e.get('responseElements') or {}).get('containerInstance') or {}).get('containerInstanceArn')
        if not arn or (keep is not None and perfrun.instance_id(arn) not in keep):
            continue
        doc = json.loads(e['requestParameters']['instanceIdentityDocument'])
        out[perfrun.instance_id(arn)] = dict(ec2=doc['instanceId'], type=doc['instanceType'], pending=doc['pendingTime'],
                                             registered=e['eventTime'])
    json.dump(out, sys.stdout, indent=1)
    print(f'ctmap: {len(out)} container instances', file=sys.stderr)


if __name__ == '__main__':
    main()
