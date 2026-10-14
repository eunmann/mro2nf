# AWS Batch run analysis

These scripts break a finished `awsbatch` run's wall time into its critical path and per-task overheads.
They also compute the run's floors and rank where the gap above those floors goes.

They need Python 3 (standard library only) and AWS CLI v2. Every AWS call is read-only and runs in
the CLI's configured region and credentials. The fetch step needs the run's S3 work directory, which
the example stack expires after 14 days.

The trace, `.nextflow.log` and `aws s3 ls` record local time. The scripts compare them with Batch's
epoch times in local time, so run every step under the `TZ` of the host that ran Nextflow.

## Pipeline

One directory per run holds its inputs and everything derived from them. Its name labels the run in
cross-run tables.

```bash
P=scripts/aws/perf
R=runs/7                                      # the run directory; its name, 7, is the run's label
$P/fetch_run.sh $R <launch dir>/trace-*.txt <launch dir>/.nextflow.log
python3 $P/instances.py $R                    # within hours of the run, for ECS and EC2; CloudTrail after that
python3 $P/phases.py $R
python3 $P/stagein.py $R
python3 $P/cp.py $R                           # the critical path; prints its components
python3 $P/analyze.py -o out $R               # add more run directories to compare runs
```

Optional steps:

```bash
python3 $P/queue_decomp.py $R                 # splits queue time into scale-up, boot, pull, start; needs ecs_tasks.json
python3 $P/cw.py $R <task ID>...              # the tasks' CloudWatch log lines against their start and stop
python3 $P/ncsplit.py -o out $R...            # splits nc into pre, stage-in, unstage, tail
python3 $P/rt_compare.py -o out <run A> <run B>   # compute per task, between two runs
python3 $P/ebs_fetch.py $R                    # root-volume metrics, within two weeks of the run
python3 $P/ebs_report.py -o out --gp3 7=1000,6000 $R   # load against the volume's MiB/s and IOPS
```

## Inputs

`fetch_run.sh RUN_DIR TRACE NEXTFLOW_LOG` writes these. The work directory comes from the trace's
`workdir` column.

| File | Source |
|---|---|
| `trace.txt`, `nextflow.log` | copies of the run's Nextflow trace and `.nextflow.log` |
| `ids`, `rels.txt` | the trace's Batch job IDs, and each task's work dir relative to the run's |
| `jobs.json` | `aws batch describe-jobs` for every job in the trace |
| `runs/<task dir>/.command.run` | each task's wrapper script, which lists the S3 downloads that stage its inputs |
| `s3ls.txt` | `aws s3 ls --recursive <work dir>/`: object sizes and LastModified times |
| `ecs_tasks.json` | `aws ecs describe-tasks`, reduced to `taskArn`, `createdAt`, `pullStartedAt`, `pullStoppedAt`, `startedAt`; ECS forgets a stopped task after about an hour |
| `cimap.json` | `aws cloudtrail lookup-events` for `RegisterContainerInstance`, through `ctmap.py` |

CloudTrail's `RegisterContainerInstance` events carry the registering instance's temporary STS
credentials in `responseElements`. `fetch_run.sh` pipes the events straight into `ctmap.py`, and never
writes them to disk. `ctmap.py` keeps, per container instance of the run, only the EC2 instance ID,
instance type, `pendingTime` and registration time. `CT_START` (ISO 8601) sets the start of the lookup.
By default it starts a day before the run's first job.

## Outputs

| Step | Writes (in the run directory unless `-o`) | What it holds |
|---|---|---|
| `instances.py` | `instances.json`, `instance_types.json` | each container instance's EC2 instance, type and registration time; the types' EBS specs |
| `phases.py` | `tasks.json` | per task: Batch `queue`, in-container time, `nc` (in-container time less the trace's `realtime`), `poll` lag, instance |
| `stagein.py` | `stagein.json` | per task: the files it stages and the tasks that wrote them, bytes in and out |
| `cp.py` | `cp_log.json` (`--end trace`: `cp.json`) | the critical path, each task split into dispatch, queue, nc, rt and poll |
| `analyze.py` | `cp_summary.csv`, `floors.json`, `gap_detail.csv`, `nc_residual.csv`, `analysis.json`; with `--pair`, `pair_heavy.csv`, `pair_by_itype.csv` | the cross-run tables, printed as markdown |
| `queue_decomp.py` | `queue_decomp.csv` | per task: Batch placement, instance launch and boot for a cold start, image pull, container start |
| `cw.py` | `cw/<task ID>.json` | the task's log events |
| `ncsplit.py` | `ncsplit.csv` | per task: nc split by its work dir's object times |
| `rt_compare.py` | `rt_compare.csv` | per task: compute in both runs, instance type, co-running tasks |
| `ebs_fetch.py` | `ebs_raw.json` | AWS/EBS 1-minute metrics of each instance's root volume |
| `ebs_report.py` | `ebs_minutes.csv`, `ebs_volumes.csv` | per volume and minute: MiB/s, IOPS, limit checks, the tasks running |

`analyze.py`'s options:

- `--baseline LABEL`: a run from before the change being measured. It appears in the critical-path,
  poll and launch tables, and stays out of the minima, floors and gap ranking.
- `--requeue TARGET=SOURCE:PROCESS`: backs an outside queue block out of `TARGET`. Its `PROCESS`
  tasks take `SOURCE`'s queue times, and `TARGET` is reported again as `TARGETadj`.
- `--pair A,B` compares the tasks that move over 1 GB in runs `A` and `B`. `--watch` adds named
  processes to that table, and `--volume-mibps LABEL=N` fills its root-volume column.
- `--wrap-floor` (default 5.5 s) and `--poll-floor` (1.0 s) set the per-task floors.
