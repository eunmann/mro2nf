# Profiles

One project runs on every target, and `-profile <name>` picks the target.

| Profile | Runs on | Needs |
|---|---|---|
| `standard` (default) | the local host | `mre`, `mrjob`, Python 3 and the pipeline's code on the host |
| `slurm`, `sge`, `lsf`, `pbs` | a grid scheduler | the same, on a filesystem shared with the compute nodes |
| `docker` | local Docker, each task in its own container | `--mro2nf_image` |
| `awsbatch` | AWS Batch, over an S3 work directory | `--mro2nf_image` in ECR, `-work-dir s3://…`, `process.queue` and `aws.region` |
| `healthomics` | AWS HealthOmics | `./package.sh <image>@sha256:<digest>` |

A container profile launched without `--mro2nf_image` stops before any task
runs. [Container images](container-images.md) shows how to build the image.
[`deploy/awsbatch-cdk`](../../deploy/awsbatch-cdk/README.md) deploys the AWS
resources both AWS profiles need, and walks through a run on each.

## Local and grid

```bash
nextflow run main.nf                     # standard
nextflow run main.nf -profile slurm      # or sge, lsf, pbs
```

These profiles run the host's `mre` and the pipeline's code in place, and read
absolute file inputs where they are ([Files and paths](files-and-paths.md)).

## Docker

```bash
nextflow run main.nf -profile docker --mro2nf_image <image>
```

The profile runs each container as the invoking user, with `HOME=/tmp`, so the
files a run leaves in `work/` are yours to delete. It sets
`process.containerOptions = '-u $(id -u):$(id -g) -e HOME=/tmp'`, which the
task's shell expands.

- A `docker.runOptions` in a `-c` config adds to that.
- A `process.containerOptions` in a `-c` config replaces it, and with it the
  user mapping.
- On **rootless Docker**, set `process.containerOptions = ''`: the
  container's root already is the invoking user, and `-u` would map to a
  subuid that cannot write `work/`. The image has no passwd entry for that
  uid, so a stage that looks up its user name (`whoami`) fails.

The `awsbatch` and `healthomics` profiles set no user: their containers run on
AWS's own hosts, not under your account.

## AWS Batch

Push the pipeline's image to ECR, built for the compute environment's
architecture (`docker build --platform linux/amd64` for x86 instances). Name
the queue and region in a config:

```groovy
// batch.config
process.queue = '<job queue>'
aws.region = '<region>'
```

```bash
nextflow run main.nf -profile awsbatch -c batch.config \
    --mro2nf_image <account>.dkr.ecr.<region>.amazonaws.com/<repo>:<tag> \
    -work-dir s3://<bucket>/work \
    -output-dir s3://<bucket>/results
```

- Keep `-output-dir` on S3: the default `results` publishes to the launch
  host.
- Pass file inputs as `s3://` URIs. The profile refuses head-node paths.
- The profile limits job submission and S3 transfers so a wide fan-out does
  not throttle the AWS APIs, and retries a task whose Spot instance is
  reclaimed.
- It checks job status every 2 s (`executor.$awsbatch.pollInterval`), where
  Nextflow's AWS Batch default is 10 s.

### Reading S3 inputs in place

Nextflow stages `s3://` file inputs into each task that reads them, as on every
profile. Where the compute environment's instances mount an input's bucket
read-only at `/mnt/s3/<bucket>`, as the example stack's do with Mountpoint for
S3, the profile reads the input in place instead:

- it bind-mounts `/mnt/s3` into every container, read-only;
- a stage receives `s3://<bucket>/<key>` as `/mnt/s3/<bucket>/<key>`.

That is one path in every task, as `mrp` passes every stage the path it was
given. So a stage may record it in an output for another stage to open, and no
task downloads it. A path under `/mnt/s3` given directly is read the same way.

The mount is an optimization, and no run needs it:

- **Each input is decided at run start.** The run's first task checks each
  input: an input whose bucket is a directory at `/mnt/s3/<bucket>` on its
  instance is read in place, and any other is staged.
- **When you need it.** A compute environment without the mounts runs every
  pipeline whose stages open their inputs by the paths they are given. A
  stage that records an input's path in an output for another task to open,
  as Cell Ranger does with its FASTQ directory, needs the input's bucket
  mounted.
- **Permissions.** A staged input's bucket must be one the instances' IAM role
  may read. The example stack mounts its work bucket and the buckets its
  `mountBuckets` context names, and its instances may read only those, so on
  it every input bucket goes in `mountBuckets`.
- **Another root.** `--mro2nf_s3_mount` names another root, which the profile
  then bind-mounts instead.
- **Every instance must mount the same buckets**: the first task's instance
  decides for every task. A `-resume` re-runs that first task, so a run after
  the mounts change reads the inputs as they now are, and keeps the other
  tasks cached while its decisions stand.
- **Missing keys.** On a key no object under a mounted bucket has, the run
  fails at its start, naming the input.
- **Read-only.** A stage that writes into an input's directory fails.
- **`-resume` keys a mounted input on its path**, not its content: a run over
  an object replaced at the same key reuses the cached tasks. Put new data
  under a new key, or run without `-resume`.
- **Path limits.** Mountpoint cannot show a key with an empty, `.` or `..`
  path component, so the run refuses such a URI of a mounted bucket.

## AWS HealthOmics

HealthOmics takes no `-c`, and applies the `standard` profile, the local one,
to a run that names no profile. So `package.sh` writes the workflow's own
`nextflow.config` instead of the project's: `conf/base.config`, the image
pinned by digest, and the `healthomics` profile's settings, with no profiles.
Every run then grants jobs as `mrp`'s cluster job managers do, and no task is
clamped to the engine's host.

```bash
IMAGE=$(docker inspect --format '{{index .RepoDigests 0}}' <repo>:<tag>)
./package.sh "$IMAGE"            # writes workflow.zip
WF=$(aws omics create-workflow --engine NEXTFLOW --main main.nf \
    --definition-zip fileb://workflow.zip \
    --parameter-template file://parameter-template.json \
    --query 'id' --output text)
aws omics start-run --workflow-id "$WF" --role-arn <role ARN> \
    --output-uri s3://<bucket>/omics-out
```

- **No executor.** The package sets none, as HealthOmics runs every task
  itself.
- **No profiles.** HealthOmics refuses a run whose engine settings name a
  profile, as the package defines none.
- **Settings.** Settings added to the project's `nextflow.config` stay out of
  the package; add them to `conf/base.config` or `conf/healthomics.config`.
- **Parameters.** `parameter-template.json` declares each pipeline input and
  `mro2nf_overrides` (an `s3://` URI), all optional. A run can set nothing
  else, so it cannot change the image or the inputs it may stage.
  `nextflow_schema.json` stays out of the zip: HealthOmics would read the
  parameters it declares from it.
- **Directory inputs.** A directory input is an `s3://` prefix ending in `/`
  (`s3://<bucket>/fastqs/`): `start-run` checks each URI and refuses a prefix
  without the slash with `S3 object not found`.
- **Outputs.** `conf/healthomics.config` sets
  `outputDir = '/mnt/workflow/pubdir'`, the directory HealthOmics exports. The
  outputs land at the root of the run's folder under the output URI:
  `<output-uri>/<run-id>/pipeline_outs.json`, and each entry of `mrp`'s `outs/`
  tree at its own path below it, such as `<output-uri>/<run-id>/d/f0.txt`.
  Earlier mro2nf versions published below `<output-uri>/<run-id>/pubdir/`.

[Targets](../architecture/targets.md) explains how the profiles are built.
