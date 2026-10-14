#!/usr/bin/env python3
"""Print tasks' CloudWatch log events against their Batch start and stop, to see where in-container time goes.

Fetches each task's log stream once into RUN_DIR/cw/<task ID>.json and reuses it. Calls the AWS CLI
read-only, in its configured region.

usage: cw.py [--log-group /aws/batch/job] RUN_DIR TASK_ID...   (reads tasks.json)
"""
import argparse
import json
import os
import subprocess
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import perfrun


def fetch(stream, group):
    """Every event of the stream: get-log-events pages forward until nextForwardToken repeats."""
    evs, token = [], None
    while True:
        cmd = ['aws', 'logs', 'get-log-events', '--log-group-name', group, '--log-stream-name', stream,
               '--start-from-head', '--output', 'json'] + (['--next-token', token] if token else [])
        res = subprocess.run(cmd, capture_output=True, text=True)
        if res.returncode:
            sys.exit(f'cw: get-log-events {stream}: {res.stderr.strip()}')
        page = json.loads(res.stdout)
        evs += page['events']
        if page['nextForwardToken'] == token:
            return evs
        token = page['nextForwardToken']


def events(run_dir, tid, stream, group):
    path = os.path.join(run_dir, 'cw', f'{tid}.json')
    if not os.path.exists(path):
        evs = fetch(stream, group)
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, 'w') as f:
            json.dump({'events': evs}, f)
    with open(path) as f:
        return json.load(f)['events']


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument('--log-group', default='/aws/batch/job')
    ap.add_argument('run_dir')
    ap.add_argument('task_ids', nargs='+')
    a = ap.parse_args()
    tasks = perfrun.tasks_by_id(a.run_dir)
    for tid in a.task_ids:
        t = tasks[tid]
        ev = events(a.run_dir, tid, t['ls'], a.log_group)
        st = perfrun.iso(t['started']).timestamp() * 1000
        sp = perfrun.iso(t['stopped']).timestamp() * 1000
        print(f"== {tid} {t['short']} incont={t['incont']:.1f} rt={t['rt']} events={len(ev)}")
        for e in ev:
            print(f"   +{(e['timestamp']-st)/1000:7.2f}s  (stop-{(sp-e['timestamp'])/1000:6.2f}s) {e['message'][:160]}")


if __name__ == '__main__':
    main()
