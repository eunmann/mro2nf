#!/usr/bin/env bash
# Fetch what the analysis needs about one AWS Batch run into RUN_DIR: a copy of the trace and .nextflow.log,
# the Batch job records, every task's .command.run, the work-dir listing, the ECS task records and the
# CloudTrail container-instance registrations. AWS CLI calls are read-only, in the CLI's configured region.
#
# usage: fetch_run.sh RUN_DIR TRACE NEXTFLOW_LOG
#   The work dir comes from the trace's workdir column. CT_START (ISO 8601) is where the CloudTrail lookup
#   starts, by default a day before the run's first job. ECS keeps a stopped task's record for about an hour,
#   so queue_decomp.py needs this run within the hour; the other steps need the work dir and the trail.
set -euo pipefail
[ $# -eq 3 ] || { sed -n '5,8p' "$0" >&2; exit 2; }
O=$1 TRACE=$2 NFLOG=$3
HERE=$(cd "$(dirname "$0")" && pwd)
mkdir -p "$O/jobparts" "$O/runs"
cp "$TRACE" "$O/trace.txt"
cp "$NFLOG" "$O/nextflow.log"

column() {
    awk -F'\t' -v c="$1" 'NR == 1 { for (i = 1; i <= NF; i++) if ($i == c) n = i; if (!n) exit 1; next } { print $n }' \
        "$O/trace.txt" || { echo "fetch_run: the trace has no $1 column" >&2; exit 1; }
}
column native_id >"$O/ids"
WORKDIRS=$(column workdir)
WORK=$(sed -E 's#/[0-9a-f]{2}/[0-9a-f]{30}/?$##' <<<"$WORKDIRS" | sort -u)
[ "$(wc -l <<<"$WORK")" -eq 1 ] || { echo "fetch_run: the trace names more than one work dir" >&2; exit 1; }
awk -v p="$WORK/" '{ print substr($0, length(p) + 1) }' <<<"$WORKDIRS" >"$O/rels.txt"

rm -f "$O"/jobparts/p_*
split -l 100 "$O/ids" "$O/jobparts/p_"
for f in "$O"/jobparts/p_??; do
    # shellcheck disable=SC2046 # one job ID per word
    aws batch describe-jobs --jobs $(cat "$f") --output json >"$f.json"
done
python3 -I -c '
import glob, json, sys
o = sys.argv[1]
jobs = [x for f in sorted(glob.glob(o + "/jobparts/p_??.json")) for x in json.load(open(f))["jobs"]]
json.dump(jobs, open(o + "/jobs.json", "w"))
print("jobs", len(jobs), "missing", len(set(open(o + "/ids").read().split()) - {x["jobId"] for x in jobs}))
' "$O"

xargs -P 16 -I{} sh -c 'mkdir -p "$1/runs/$3" && aws s3 cp --quiet "$2/$3/.command.run" "$1/runs/$3/.command.run"' \
    _ "$O" "$WORK" {} <"$O/rels.txt"
echo "command.run files: $(find "$O/runs" -name .command.run | wc -l)"
aws s3 ls --recursive "$WORK/" >"$O/s3ls.txt"
echo "s3 objects: $(wc -l <"$O/s3ls.txt")"

# One "cluster taskArn..." line per 100 ECS tasks; describe-tasks takes at most 100.
python3 -I -c '
import json, sys
by = {}
for j in json.load(open(sys.argv[1])):
    for a in j.get("attempts") or []:
        arn = (a.get("container") or {}).get("taskArn")
        if arn:
            by.setdefault(arn.split("/")[1], []).append(arn)
for c, arns in by.items():
    for i in range(0, len(arns), 100):
        print(c, *arns[i:i + 100])
' "$O/jobs.json" >"$O/ecsparts"
: >"$O/ecs_tasks.jsonl"
while read -r cluster arns; do
    # shellcheck disable=SC2086 # one task ARN per word
    aws ecs describe-tasks --cluster "$cluster" --tasks $arns --output json | python3 -I -c '
import json, sys
keep = ("taskArn", "createdAt", "pullStartedAt", "pullStoppedAt", "startedAt")
for t in json.load(sys.stdin)["tasks"]:
    print(json.dumps({k: t.get(k) for k in keep}))
' >>"$O/ecs_tasks.jsonl"
done <"$O/ecsparts"
python3 -I -c '
import json, sys
tasks = [json.loads(line) for line in open(sys.argv[1])]
json.dump(tasks, open(sys.argv[2], "w"), indent=1)
print("ecs tasks", len(tasks))
' "$O/ecs_tasks.jsonl" "$O/ecs_tasks.json"
rm -f "$O/ecsparts" "$O/ecs_tasks.jsonl"

START=${CT_START:-$(python3 -I -c '
import datetime as dt, json, sys
ms = min(j["createdAt"] for j in json.load(open(sys.argv[1]))) - 86400000
print(dt.datetime.fromtimestamp(ms / 1000, dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"))
' "$O/jobs.json")}
aws cloudtrail lookup-events --start-time "$START" --output json \
    --lookup-attributes AttributeKey=EventName,AttributeValue=RegisterContainerInstance |
    python3 -I "$HERE/ctmap.py" --jobs "$O/jobs.json" >"$O/cimap.json"
