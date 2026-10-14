#!/usr/bin/env python3
"""Fetch the AWS/EBS 1-minute metrics of the root volumes of a run's instances.

A volume is matched to an instance through the per-attachment metrics (VolumeIOPSExceededCheck and the like),
whose dimensions are {VolumeId, InstanceId}. CloudWatch lists a metric only while it has data from the last
two weeks, so fetch within that. The window defaults to the run's first job creation to its last job stop.
Calls the AWS CLI read-only, in its configured region.

usage: ebs_fetch.py RUN_DIR [START END]   (ISO 8601 UTC; reads instances.json, jobs.json; writes ebs_raw.json)
"""
import argparse
import datetime as dt
import json
import os
import subprocess
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import perfrun

VOL_METRICS = {'VolumeReadBytes': 'Sum', 'VolumeWriteBytes': 'Sum', 'VolumeReadOps': 'Sum', 'VolumeWriteOps': 'Sum',
               'VolumeQueueLength': 'Average', 'VolumeIdleTime': 'Sum', 'VolumeTotalReadTime': 'Sum',
               'VolumeTotalWriteTime': 'Sum', 'VolumeThroughputPercentage': 'Average',
               'VolumeConsumedReadWriteOps': 'Sum'}
ATT_METRICS = {'VolumeIOPSExceededCheck': 'Maximum', 'VolumeThroughputExceededCheck': 'Maximum',
               'VolumeStalledIOCheck': 'Maximum', 'VolumeAvgIOPS': 'Maximum', 'VolumeAvgThroughput': 'Maximum',
               'VolumeAvgReadLatency': 'Maximum', 'VolumeAvgWriteLatency': 'Maximum'}


def aws(*a):
    r = subprocess.run(['aws', *a, '--output', 'json'], capture_output=True, text=True, check=True)
    return json.loads(r.stdout)


def window(run_dir):
    jobs = perfrun.read_json(run_dir, 'jobs.json')

    def utc(ms):
        return dt.datetime.fromtimestamp(ms / 1000, dt.timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ')

    return utc(min(j['createdAt'] for j in jobs)), utc(max(j['stoppedAt'] for j in jobs) + 60000)


def queries(listed, inst):
    by_ec2 = {i['ec2']: (k, i) for k, i in inst.items()}
    vmap = {}
    for m in listed:
        d = {x['Name']: x['Value'] for x in m['Dimensions']}
        if m['MetricName'] == 'VolumeIOPSExceededCheck' and d.get('InstanceId') in by_ec2:
            k, i = by_ec2[d['InstanceId']]
            vmap[d['VolumeId']] = dict(ec2=d['InstanceId'], ci=k, type=i['type'], pending=i['ct_pending'])
    have = {(m['MetricName'], tuple(sorted((x['Name'], x['Value']) for x in m['Dimensions']))) for m in listed}
    q = []
    for v, x in vmap.items():
        for name, stat in {**VOL_METRICS, **ATT_METRICS}.items():
            dims = [('VolumeId', v)] + ([('InstanceId', x['ec2'])] if name in ATT_METRICS else [])
            if (name, tuple(sorted(dims))) in have:
                q.append(dict(Id=f'q{len(q)}', Label=f'{v}|{name}', ReturnData=True, MetricStat=dict(
                    Metric=dict(Namespace='AWS/EBS', MetricName=name, Dimensions=[dict(Name=a, Value=b) for a, b in dims]),
                    Period=60, Stat=stat)))
    return vmap, q


def fetch(q, start, end):
    raw = {}
    for k in range(0, len(q), 400):
        tok = None
        while True:
            d = aws('cloudwatch', 'get-metric-data', '--start-time', start, '--end-time', end,
                    '--metric-data-queries', json.dumps(q[k:k + 400]), *(['--next-token', tok] if tok else []))
            for r in d['MetricDataResults']:
                v, name = r['Label'].split('|')
                raw.setdefault(v, {}).setdefault(name, {}).update(dict(zip(r['Timestamps'], r['Values'])))
            tok = d.get('NextToken')
            if not tok:
                break
    return raw


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument('run_dir')
    ap.add_argument('window', nargs='*', metavar='START END')
    a = ap.parse_args()
    if len(a.window) not in (0, 2):
        ap.error('give both START and END, or neither')
    start, end = a.window or window(a.run_dir)
    inst = perfrun.read_json(a.run_dir, 'instances.json')
    vmap, q = queries(aws('cloudwatch', 'list-metrics', '--namespace', 'AWS/EBS')['Metrics'], inst)
    raw = fetch(q, start, end)
    missing = sorted((set(VOL_METRICS) | set(ATT_METRICS)) - {x['Label'].split('|')[1] for x in q})
    perfrun.write_json(a.run_dir, 'ebs_raw.json', dict(window=[start, end], metrics_not_published=missing, map=vmap, raw=raw))
    print(f'{len(inst)} instances, {len(vmap)} volumes mapped; metrics not published for these volumes: {missing}')
    for v, x in sorted(vmap.items(), key=lambda kv: kv[1]['pending'] or ''):
        print(' ', v, x['ec2'], x['ci'][:8], x['type'], x['pending'], 'datapoints',
              len(raw.get(v, {}).get('VolumeWriteOps', {})))


if __name__ == '__main__':
    main()
