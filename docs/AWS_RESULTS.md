# Live AWS validation and performance

This is the record of mro2nf's live runs on AWS Batch over S3 and on AWS
HealthOmics, 2026-10-12 to 2026-10-14. It covers what was checked, the
CellRanger critical-path analysis, what each change bought, and the
options that were ruled out. [`RUNNING.md`](RUNNING.md) holds the
user-facing rules these runs established.

## Environment

- **Account and region:** us-east-1 (HealthOmics is not offered in
  us-east-2).
- **Stack:** `Mro2nfStack`, from
  [`deploy/awsbatch-cdk`](../deploy/awsbatch-cdk/README.md):
  - a Spot queue (`SPOT_PRICE_CAPACITY_OPTIMIZED`, optimal instance
    classes, `minvCpus` 0, `maxvCpus` 256);
  - the work bucket mounted read-only at `/mnt/s3/<bucket>` with
    Mountpoint for S3 1.24.0;
  - a 500 GiB gp3 root volume.
- **Images:** `deploy/runtime/Dockerfile`'s, plus `testdata/` and
  `bench/` for the fixtures, or the CellRanger 10.1.0 bundle.
- **Nextflow:** 26.04.7 on the driver. HealthOmics ran 25.10.0, then
  26.04.0 after the 26.04 floor (#584).
- **Cost:** about $3.50 through 2026-10-13; under $15 estimated in all.

## What was validated

| Check | Date | Build | Result |
|---|---|---|---|
| Batch fixture matrix: 40 fixtures, `diff -r` of `results/` against a local run of each | 2026-10-13 | `e1afb3d` | 36 byte-identical; 4 expected differences (below) |
| `-resume` over S3 on Batch: `split_test`, `file_tree`, `split_forward_file`, `map_call_entry_override` | 2026-10-13 | `e1afb3d` | every stage task cached; only PLAN re-ran, as it does when a mount root is set (#534) |
| `-output-dir s3://…` on Batch: `split_test` | 2026-10-14 | #586's branch, runtime `e1afb3d` | published tree byte-identical to local |
| HealthOmics: 9 fixtures | 2026-10-13 | `e1afb3d` | byte-identical to local |
| HealthOmics: cluster rules for job resources (#571) | 2026-10-13 | #572 | fixed and validated live; 2 fixtures re-run on the #572 package, byte-identical |
| HealthOmics on engine 26.04.0: 4 fixtures | 2026-10-14 | #584's branch | byte-identical |
| HealthOmics probes for #580 and #357 | 2026-10-14 | #586's branch | no undeclared params passed, so the `params {}` guard is safe; outputs under `<output-uri>/<run-id>/` |
| CellRanger 10.1.0 `count` (pbmc_1k_v3) on Batch, runs 3, 4, 6, 7 | 2026-10-12 to 2026-10-14 | `60e9ab5` (run 3), `e1afb3d` | 153/153 tasks each; outputs identical across runs (below) |
| CellRanger run 5, inputs not mounted | 2026-10-13 | `e1afb3d` | failed at `MAKE_SHARD` on a path written into text (#546; [below](#requirements-users-must-know)) |

The four expected differences in the matrix:

- `assert_split` and `stage_raise` are meant to fail, and failed locally
  and on Batch.
- `res_pool` requests 64 vCPU and 200 GB, which no instance in the queue
  can hold (see #570 below).
- `overrides_resources` shows Batch's cluster rules (threads 2) against
  the local rules (1.25), as #556 intends.

The matrix covered file references (#562: `file_tree`,
`split_forward_file`, `shared_out_file`), the top-level map call
(`map_call_entry` with its own values, and again as
`map_call_entry_override` with `{"split":[4,5]}`), `gate_chain`,
`map_file_2d`, `forkdisabled`, and staging when the root is not mounted
(`entry_filearr` and `entry_dir` again, as `entry_filearr_staged` and
`entry_dir_staged`, with `--mro2nf_s3_mount` naming an unmounted root).

CellRanger's outputs matched across runs 3, 4, 6 and 7. 36 of 41 files
were byte-identical. The 3 gzipped matrices hold identical content and
differ only in their gzip header timestamps. `metrics_summary.csv` was
identical, and `web_summary.html` and the `cloupe` file carry per-run
values.

## CellRanger critical path

The critical path is the longest chain of dependent tasks, from PLAN to
OUTS. It sets the wall time. Each task on it splits into these parts:

- **Dispatch:** from the previous task's end to this job's creation.
- **Queue:** Batch scheduling, instance launch and image pull.
- **nc:** in-container time other than compute: staging and the
  wrapper.
- **Compute:** the trace's `realtime`.
- **Poll:** how long Nextflow takes to notice that the job finished.

| Run | Date | Code | Root volume | Path | Queue | nc | Compute | Poll | Dispatch |
|---|---|---|---|---:|---:|---:|---:|---:|---:|
| 3 | 2026-10-12 | `60e9ab5`, before epic #533 | gp3 125 MiB/s | 4,190 s | 2,113 | 948 | 656 | 352 | 121 |
| 4 | 2026-10-13 | `e1afb3d` | gp3 125 MiB/s | 3,367 s\* | 1,729 | 755 | 681 | 88 | 112 |
| 6 | 2026-10-13 | `e1afb3d` | gp3 2,000 MiB/s, 80,000 IOPS | 2,604 s | 1,193 | 638 | 573 | 87 | 112 |
| 7 | 2026-10-14 | `e1afb3d` | gp3 1,000 MiB/s, 4,000 IOPS | 2,590 s | 1,225 | 618 | 540 | 88 | 118 |

\* Run 4 measured 7,459 s. `res_pool`, a matrix job no instance could
hold, blocked the queue ahead of it for 66–75 minutes. The table backs
the block out by giving the four affected `ALIGN_AND_COUNT` chunks run
3's queue times (`analyze.py --requeue`).

Other measures:

- **Per-task overhead** (nc, all 153 tasks), run 3 to run 4: the sum went
  from 2,133 to 1,621 s (−24%) and the median from 11.3 to 7.4 s (−35%).
- **Per-task floor:** 5.5 s, the AWS CLI calls in Nextflow's Batch
  wrapper. This is inherent to the executor.
- **Launch to PLAN submitted:** 98 s (run 3), 81 s (run 4), 20 s (run 6).
  Runs 4 and 6 ran the same project; run 4's driver shared its host with
  the fixture matrix.
- **Short tasks:** 46 of run 6's 65 path tasks compute for under 2 s, and
  31 of those are split or join phases. Each costs about 20 s of Batch
  overhead. `mrp` also runs each phase as its own job.

### Floors

- **Theoretical floor:** how long the run would take if every task cost
  only its own compute and started the moment its inputs existed. That
  is the longest chain of compute alone: 728 s for run 6, 28% of its
  wall time. No executor reaches this floor.
- **Practical floor:** the best that open-source Nextflow on Batch can do
  for this pipeline. Run 6's is 1,326 s, 51% of its wall time. The floor
  charges each task on the chain:
  - its compute;
  - about 8.85 s of overhead: the 5.5 s wrapper floor, 0.9 s dispatch,
    1 s average poll and 1.5 s warm queue, the least seen;
  - the bytes it must move, at 455 MB/s, the best staging rate seen.

  The run also pays one cold start, for PLAN's instance (140 s).

Run 6's floors use the minima of runs 4 and 6. Against the minima of runs
6 and 7, run 7's theoretical floor is 720 s (27.8% of its wall time) and
its practical floor 1,335 s (51.5%). Those minima move run 6's practical
floor to 1,380 s.

### Where the gap goes

Run 6 is 1,278 s above its practical floor. The causes below sum to
1,293 s, which is the gap measured against the floor of the measured
path (1,311 s).

| Cause | Seconds | Owner |
|---|---:|---|
| Mid-run cold starts: Batch scale-up 308 s, boot 104 s, image pull 50 s | 542 | infrastructure |
| Queue above the minimum for 58 short tasks (ECS task creation, about 5 s each) | 417 | inherent to Batch |
| nc above the floor that staging does not explain, and packing | 144 | unattributed |
| Intermediates staged but not read | 81 | was 194 s before gp3 (run 4) |
| Dispatch above the minimum | 54 | inherent |
| Staging below the best rate | 33 | unattributed |
| Poll above 1 s | 22 | configuration |

## What each change bought

| Change | Effect | Evidence |
|---|---|---|
| Poll Batch every 2 s instead of 10 s (#547, issue #537) | Path poll lag 352 → 88 s; per task 5.33 → 1.33 s (run 3 → 4) | `analysis/perf/analysis.md` |
| Fewer staged items: epic #533, among them small values carried as channel values instead of staged bundles (#563, issue #536) and args and outs handed over in memory (#564, issue #542) | S3 downloads 1,205 → 1,084 per run, median per task 7 → 5; per-task nc sum 2,133 → 1,621 s, median 11.3 → 7.4 s (run 3 → 4, the epic as a whole) | `stagein.json`, `tasks.json` per run |
| Pass-through files named by reference, not re-uploaded (#564, issue #562) | Removes the re-upload of a file a stage passes through, on every executor; not measured apart from epic #533 | #544's measurement comment |
| Root volume at gp3's maxima (#573), then 1,000 MiB/s and 6,000 IOPS (#581) | `MAKE_SHARD` join nc 56.5 → 18.3 s (455 MB/s); `ALIGN_AND_COUNT` split 49.9 → 19.9 s, join 53.1 → 22.9 s; preflights 24 → 6 s (run 4 → 6) | `analysis/perf/analysis.md` |
| Cancel a job no instance can fit after 10 minutes (`jobStateTimeLimitActions`, #570); a job waiting for Spot capacity still waits | A job like `res_pool` fails its run instead of holding the queue; it cost run 4 4,092 s of path time | `analysis/perf/analysis.md` |

The gp3 runs show how the default was chosen. Runs 3 and 4 copied at the
default 125 MiB/s: 5.8 GB at 114–130 MB/s. Run 7, at 1,000 MiB/s and
4,000 IOPS, matched run 6 at the maxima (2,590 s against 2,604 s).
Staging tops out at about 400–500 MB/s, the S3 download rate, and
throughput never neared 1,000 MiB/s. CloudWatch shows IOPS at the 4,000
limit for 7 volume-minutes. Six fell during `ALIGN_AND_COUNT`'s stage-in
and run, and one during the preflights' Mountpoint cache fill. Writes ran
at about 200 KiB per operation, and the time at the limit had no
measurable cost (`analysis/perf/r7/ebs_report.md`).

## Requirements users must know

- **CellRanger on Batch needs its inputs mounted.** CellRanger writes an
  absolute path inside a task's own work directory into a JSON value, and
  a later task opens it. Run 5, with the mount turned off, failed at
  `MAKE_SHARD` on that path. Paths written into text are out of scope
  (#546; [Paths written into text](RUNNING.md#paths-written-into-text)).
  HealthOmics runs it as is, since its run storage is shared.
- **HealthOmics output layout:** the outputs land at
  `<output-uri>/<run-id>/`. Earlier versions wrote them to
  `<run-id>/pubdir/` (#586; [HealthOmics](RUNNING.md#healthomics)).
- **gp3 root volume:** the example stack provisions 1,000 MiB/s and
  6,000 IOPS. This costs about $0.07 per instance-hour above gp3's
  baseline, against $0.63 at the maxima. Set other values with
  `-c rootThroughputMiBps` and `-c rootIops`
  ([`deploy/awsbatch-cdk/README.md`](../deploy/awsbatch-cdk/README.md#cost)).

## Optimizations evaluated and ruled out

- **Lazy intermediates through the work-bucket mount (#544):** at most
  about 12% of the path (509 s) before gp3, and 81 s after. It would add
  a second data path, breaking the rule that a task reads what Nextflow
  stages into it.
  [Measurement](https://github.com/eunmann/mro2nf/issues/544#issuecomment-6151590454).
- **Fusion and Wave:** licensed, so out of scope.
- **JuiceFS, EFS, FSx:** open-source Nextflow's `awsbatch` executor
  requires an `s3://` work dir
  ([`AwsBatchExecutor.validateWorkDir`](https://github.com/nextflow-io/nextflow/blob/36e08de58bab0e9c246f79238088451f7b5222fa/plugins/nf-amazon/src/main/nextflow/cloud/aws/batch/AwsBatchExecutor.groovy#L117-L125)).
  EFS and FSx need Seqera's licensed `xpack-amzn`. On the grid profiles,
  any shared POSIX filesystem already works unchanged.
- **Other FUSE clients** (s3fs, goofys, geesefs, rclone): none beats
  Mountpoint for read-only lazy reads, and none avoids uploads (the
  clients' documentation, `research/lazy-s3/` in the archive).
- **Nextflow settings:** `stageInMode` does not apply on Batch, which
  always stages with `nxf_s3_download`
  ([`AwsBatchFileCopyStrategy`](https://github.com/nextflow-io/nextflow/blob/36e08de58bab0e9c246f79238088451f7b5222fa/plugins/nf-amazon/src/main/nextflow/cloud/aws/batch/AwsBatchFileCopyStrategy.groovy#L95-L101)).
  s5cmd is used only in Fargate mode
  ([`AwsBatchConfig`](https://github.com/nextflow-io/nextflow/blob/36e08de58bab0e9c246f79238088451f7b5222fa/plugins/nf-amazon/src/main/nextflow/cloud/aws/config/AwsBatchConfig.groovy#L146)).
  `aws.client.*` tunes the driver's SDK, not the wrapper's copies. A
  native fix would be upstream symlink staging for mounted S3 paths,
  along the lines of nextflow-io/nextflow#7186.
- **Split/join fusion (#582):** fusing one stage instance's phases when
  they have identical requests is the only option that holds every
  constraint. It removes 7 of 65 path tasks, an estimated 2–5% of wall
  time. It was rejected for its complexity, its wider retries, and the
  loss of the 1:1 match with `mrp`'s jobs.
- **Deployment levers:** warm capacity (`minvCpus` above 0), a smaller
  image, and a 1 s poll cut queue and poll time. They are choices for
  each deployment, not for mro2nf.

## Reproduce or re-measure

[`scripts/aws/perf/`](../scripts/aws/perf/README.md) holds the
analysis. To measure a new Batch run, launch it as
[RUNNING.md](RUNNING.md#aws-batch) describes. The project writes
a new `trace-<timestamp>.txt` into the launch directory on each run, so
pass the run's own trace by name. Within an hour of the run, while ECS
still holds its task records, run:

```bash
P=scripts/aws/perf
$P/fetch_run.sh runs/new <launch dir>/trace-<timestamp>.txt <launch dir>/.nextflow.log
python3 $P/instances.py runs/new
python3 $P/phases.py runs/new
python3 $P/stagein.py runs/new
python3 $P/cp.py runs/new
python3 $P/queue_decomp.py runs/new
python3 $P/analyze.py -o out --pair old,new runs/old runs/new
```

Run every step under the `TZ` of the host that ran Nextflow. The
scripts' README lists each step's inputs and outputs, and the optional
EBS and log steps.

The scripts recompute the tables above from the archive
([recipe](../scripts/aws/perf/README.md#the-archived-cellranger-runs)).
Run 7's critical path comes out at 2,590 s over 66 tasks: 118.1 s
dispatch, 1,225.5 s queue, 617.7 s nc, 540.4 s compute and 88.3 s poll.

## Raw data

The archive is at
`s3://mro2nfstack-workbucketf885502b-g0pvupxxfygm/archive/2026-10-aws-validation/`,
in the example stack's private work bucket:

- `NOTES.md`: the consolidated findings this document draws on.
- `cellranger/batch<run>/`: each CellRanger run's trace, `.nextflow.log`,
  console output and `batch.config`.
- `analysis/m544/`: #544's measurement of run 3 and its derived files.
- `analysis/perf/`: runs 3, 4, 6 and 7's records (`r<run>/`), runs 3, 4
  and 6 compared, and in `r7/` runs 6 and 7 compared with EBS metrics.
- `harness/`: the matrix and HealthOmics scripts and the matrix log.
- `research/`: the Nextflow and FUSE-client sources and docs read for
  [the ruled-out options](#optimizations-evaluated-and-ruled-out).

The CloudTrail events behind `ct_cimap.json` carry instance credentials
and are not archived.
