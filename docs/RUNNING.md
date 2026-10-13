# Running a transpiled pipeline

This guide covers transpiling a pipeline, launching it, sizing and retrying its
tasks, the target profiles, GPU stages, `mrp`'s flags, and what to check when a
run fails. The [README](../README.md) has a first run and a container example.

## Transpile

```bash
mro2nf [-o <dir>] [-mropath <path>] <pipeline.mro>
```

- The entry `.mro` holds the pipeline and the `call` that `mrp` would run. The
  `call`'s arguments become the run's default inputs.
- `-o` is the project directory (default `out`).
- `-mropath` is the `@include` search path, `:`-separated. The default is
  `$MROPATH` when it is set, else the `.mro`'s directory, as under `mrp`. The
  entry's own directory is searched first for its own includes and its stages'
  code. The working directory is never searched.
- `mro2nf` fails, and writes no project, on a pipeline `mrp` would refuse to
  start, with `mrp`'s message. It also fails on the few shapes whose data the
  tasks cannot exchange; [`FEATURE_COVERAGE.md`](FEATURE_COVERAGE.md) lists
  them.

The project refers to the entry `.mro`, its includes and its stages' code by
their absolute paths at transpile time. Every profile runs the code from those
paths, so don't move them; a container image holds the code at the same paths.
That includes the entry `.mro` itself: an invocation written outside the code
tree, such as the `__<id>.mro` that `cellranger count --dry` writes, goes into
the image at its own path too.

The project holds:

| File | Purpose |
|---|---|
| `main.nf` | the entry workflow: PLAN, the root pipeline's calls, then OUTS |
| `subworkflows/` | one workflow per sub-pipeline call |
| `stage.nf` | the stage processes and the wiring every node uses |
| `nextflow.config` | parameters, the retry policy, the GPU rule, the trace, the profiles |
| `conf/` | the container and HealthOmics profiles' settings |
| `package.sh`, `parameter-template.json` | the HealthOmics package |
| `_assets/` | a copy of the `.mro` files and the type manifest, read by PLAN and OUTS |

A run writes `work/`, `results/`, `.nextflow/`, `.nextflow.log` and
`trace-<timestamp>.txt` into the launch directory.

## Launch parameters

### Pipeline inputs

Each input of the entry pipeline is a parameter of the same name. When it is
unset, the run uses the value in the `.mro`'s `call`. The run's PLAN task
compiles the call with the launch values, so a launch value changes forks,
disabled calls and chunk counts as the same value in the `.mro` would under
`mrp`.

- A scalar `int`, `float` or `bool` can be passed on the command line
  (`--n 5 --scale 2.5 --skip false`). The value must parse as the declared
  type, or the run fails.
- An array, map or struct must be passed as typed JSON in a `-params-file`
  (`{"values": [5, 6, 7]}`). On the command line it fails with `entry override
  of a non-scalar type cannot be a string; pass the value as typed JSON in a
  -params-file`.
- A file input is an absolute path or an `s3://` URI. Each task stages the
  files it reads. Other URI schemes (`http://`, `https://`, `ftp://`) are
  refused unless you pass `--mro2nf_allow_remote_inputs true`. The `awsbatch`
  and `healthomics` profiles refuse head-node paths, so give them `s3://`
  URIs.
- A stage receives a file input at a path in its own task directory, with the
  same file name and content. Under `mrp` it receives the original path. On
  an executor that isolates its tasks, a stage that writes that path into an
  output for another stage to open, rather than passing the file on as a
  file-typed output, therefore names a path the other task does not have.
  The `awsbatch` profile avoids this for `s3://` inputs by reading them in
  place through a mount ([AWS Batch](#aws-batch)).
- Only the named file is staged, not the files beside it, whether it is a
  pipeline input or another stage's output. A stage that opens a companion
  file next to its input, such as an index beside a BAM, finds it under `mrp`
  but not under mro2nf. Declare the companion as its own input, and as its
  own output of the stage that writes it, and have the stage open it from
  that input, or pass a directory input that holds both files. Neither
  applies to an input read through the `awsbatch` mount, which a stage
  receives at its mounted path, beside the rest of its bucket.
- Nextflow's v1 syntax parser, the default before 26.04 or under
  `NXF_SYNTAX_PARSER=v1`, converts command-line values before the run sees
  them: `--s 007` becomes the number `7`, which fails a `string` input. Set
  `NXF_DISABLE_PARAMS_TYPE_DETECTION=true`, or use a `-params-file`, to pass
  such strings unchanged ([`TRANSPILER.md`](TRANSPILER.md) §8).

An entry input named `outdir`, `job_resources` or `mro2nf_*` fails the
transpile, because the project defines those parameters itself.

### mro2nf parameters

| Parameter | Default | Effect |
|---|---|---|
| `--outdir <dir>` | `results` | where the outputs are published: a directory or an `s3://` prefix |
| `--mro2nf_overrides <file>` | none | `mrp`'s `--overrides` file (see [Resources](#resources)) |
| `--mro2nf_monitor true` | `false` | `mrp`'s `--monitor`: `mrjob` kills a job that exceeds its memory request |
| `--mro2nf_confine true` | `false` | fails a stage task whose output resolves outside its work directory, instead of copying the file in; the container profiles always do this |
| `--mro2nf_image <image>` | none | the image every task runs in; required by the container profiles |
| `--mro2nf_mre <path>` | `mre` on `PATH` | the `mre` the tasks run |
| `--mro2nf_mrjob <path>` | the `mrjob` beside `mre` | the `mrjob` that runs `py` and `comp` stages |
| `--mro2nf_adapters <dir>` | the adapters `mre` embeds | Martian's adapters directory, holding `python/martian_shell.py` |
| `--mro2nf_psid <id>` | the `call`'s name | the pipestance ID `mrp` takes as its second argument; it names each job in `_jobinfo` |
| `--mro2nf_allow_remote_inputs true` | `false` | allows file inputs with URI schemes other than `s3://` |
| `--mro2nf_s3_mount <dir>` | `/mnt/s3` on `awsbatch`, none elsewhere | the directory every task's host may mount input buckets under, read-only, one directory per bucket; a stage receives an `s3://` input of a bucket mounted there as its path there, and any other staged |

`--mro2nf_mre`, `--mro2nf_mrjob` and `--mro2nf_adapters` let the stages run
under another Martian runtime, such as the one bundled with a pipeline.

`params.job_resources`, set in a `-c` config, maps a stage's `special` value
to the `clusterOptions` of its tasks on a grid executor, like `mrp`'s
`MRO_JOBRESOURCES`:

```groovy
params.job_resources = [highmem: '--partition=highmem']
```

## Resources

Each task is sized as `mrp` sizes the job:

- `cpus` is the job's `threads`, rounded up, and at least 1.
- `memory` is the job's `mem_gb`, or 1 GB when unset.
- A chunk and the join use the `__threads`, `__mem_gb` and `__special` that
  the split returned for them.
- The stage sees `mrp`'s request, unrounded, in `_jobinfo` and in `__threads`,
  `__mem_gb` and `__vmem_gb`.

`--mro2nf_overrides` takes the file `mrp --overrides` takes. PLAN reads it with
Martian's reader, so a key `mrp` does not know fails the run before any stage
starts. Each job's resources then resolve as `mrp` resolves them, and reach
both the task's `cpus` and `memory` and what the stage sees:

```bash
nextflow run main.nf --mro2nf_overrides overrides.json
```

```json
{"SUM_SQUARE_PIPELINE.SUM_SQUARES": {"chunk.mem_gb": 3, "chunk.threads": 2}}
```

The `force_volatile` and `*.profile` keys are accepted and have no effect.
With `-resume`, a changed overrides file re-runs the tasks of each stage
whose resources it changes, and the tasks downstream of them.

A `withName:` selector in a `-c` config sizes one stage's tasks without
changing what the stage sees. Each stage call is a process named
`N<i>_<CALL>`, and a split stage's phases are `N<i>_<CALL>_SP`, `_MN` and
`_JN`. The `include` lines of `main.nf` and `subworkflows/` list the names. A
selector names the process inside a sub-workflow too, where the trace shows it
as `W<j>_<SUB>:N<i>_<CALL>`.

```groovy
process {
    withName: 'N3_ALIGN_MN' { memory = '8 GB'; cpus = 4 }
}
```

`-resume` does not re-run a cached task because a `-c` setting changed its
`cpus` or `memory`.

The `standard` and `docker` profiles clamp each task's request to the host's
cpus and memory (`process.resourceLimits`), so a request larger than the host
runs instead of failing, as `mrp` clamps a job to its local pool. The
`awsbatch` and grid profiles do not clamp.

## Retries

A task is retried only when `mrp` would retry the job, and at the same cpus
and memory:

- `mre` exits 75 when a line of the job's `_errors` matches a `retry_on`
  pattern of `mrp`'s `jobmanagers/retry.json`: a signal, `ENOMEM`, or a
  scheduler failure.
- A task the executor kills with a signal exits 137 or 143.
- These exits are retried up to twice, `mrp`'s `default_retries`. Any other
  failure ends the run on its first attempt: an `ASSERT` (exit 42), a stage
  exception or a failed `mre` step (exit 1).
- Under `--mro2nf_monitor true`, a job killed for exceeding its memory request
  is not retried, as under `mrp` without `--auto-adjust-memory`.

To change the number of retries, as `mrp --autoretry=N` does, set
`process.maxRetries = N` in a `-c` config. To raise the memory on each retry,
which `mrp` does only under `--auto-adjust-memory`, use this `-c` config:

```groovy
process {
    withName: 'N[0-9]+_.*' { memory = { "${meta.mem_gb * task.attempt} GB" } }
}
```

The task is still sized at the stage's request on its first attempt, and the
stage still sees that request in `_jobinfo`.

## Profiles

One project runs on every target; `-profile <name>` chooses it.

| Profile | Runs on | Needs |
|---|---|---|
| `standard` (default) | the local host | `mre`, `mrjob`, Python 3 and the pipeline's code on the host |
| `slurm`, `sge`, `lsf`, `pbs` | a grid scheduler | the same, on a filesystem shared with the compute nodes |
| `docker` | local Docker, each task in its own container | `--mro2nf_image` |
| `awsbatch` | AWS Batch, over an S3 work directory | `--mro2nf_image` in ECR, `-work-dir s3://…`, `process.queue` and `aws.region` |
| `healthomics` | AWS HealthOmics | `./package.sh <image>@sha256:<digest>` |

A container profile launched without `--mro2nf_image` stops before any task
runs. [`deploy/awsbatch-cdk/README.md`](../deploy/awsbatch-cdk/README.md)
deploys the AWS resources both AWS profiles need, and walks through a run on
each.

### Grid

```bash
nextflow run main.nf -profile slurm      # or sge, lsf, pbs
```

### Docker

```bash
nextflow run main.nf -profile docker --mro2nf_image <image>
```

The `docker` profile runs each container as the invoking user, with
`HOME=/tmp`, so the files a run leaves in `work/` are yours to delete. It sets
`process.containerOptions = '-u $(id -u):$(id -g) -e HOME=/tmp'`, which the
task's shell expands. A `docker.runOptions` in a `-c` config adds to that; a
`process.containerOptions` in a `-c` config replaces it, and with it the user
mapping. On rootless Docker, set `process.containerOptions = ''`: the
container's root already is the invoking user, and `-u` would map to a subuid
that cannot write `work/`. The image has no passwd entry for that uid, so a
stage that looks up its user name (`whoami`) fails. The `awsbatch` and
`healthomics` profiles set no user: their containers run on AWS's own hosts,
not under your account.

### AWS Batch

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
    --outdir s3://<bucket>/results
```

Keep `--outdir` on S3: the default `results` publishes to the launch host.
The profile limits job submission and S3 transfers so a wide fan-out does not
throttle the AWS APIs, and retries a task whose Spot instance is reclaimed.

Nextflow stages `s3://` file inputs into each task that reads them, as on
every profile. Where the compute environment's instances mount an input's
bucket read-only at `/mnt/s3/<bucket>`, as the example stack's do with
Mountpoint for S3, the profile reads the input in place instead: it
bind-mounts `/mnt/s3` into every container, read-only, and a stage receives
`s3://<bucket>/<key>` as `/mnt/s3/<bucket>/<key>`. That is one path in every
task, as `mrp` passes every stage the path it was given, so a stage may record
it in an output for another stage to open, and no task downloads it. A path
under `/mnt/s3` given directly is read the same way.

- The mount is an optimization, and a run needs none. The run's first task
  decides per input: an input whose bucket is a directory at
  `/mnt/s3/<bucket>` on its instance is read in place, any other is staged.
  A compute environment without the mounts runs every pipeline whose stages
  open their inputs by the paths they are given; a stage that records an
  input's path in an output for another task to open, as Cell Ranger does
  with its FASTQ directory, needs the input's bucket mounted. A staged
  input's bucket must be one the instances' IAM role may read. The example
  stack mounts its work bucket and the buckets its `mountBuckets` context
  names ([`deploy/awsbatch-cdk/README.md`](../deploy/awsbatch-cdk/README.md)),
  and its instances may read only those, so on it every input bucket goes in
  `mountBuckets`; `--mro2nf_s3_mount` names another root, which the profile
  then bind-mounts instead.
- Every instance of the compute environment must mount the same buckets: the
  first task's instance decides for every task. A `-resume` re-runs that
  first task, so a run after the mounts change reads the inputs as they now
  are, and keeps the other tasks cached while its decisions stand.
- On a key no object under a mounted bucket has, the run fails at its start,
  naming the input.
- The mount is read-only: a stage that writes into an input's directory
  fails.
- `-resume` keys a mounted input on its path, not its content: a run over an
  object replaced at the same key reuses the cached tasks. Put new data under
  a new key, or run without `-resume`.
- Mountpoint cannot show a key with an empty, `.` or `..` path component, so
  the run refuses such a URI of a mounted bucket.

### HealthOmics

HealthOmics takes no `-profile` and no `-c`. `package.sh` makes the
`healthomics` profile the workflow's own config and pins the image by digest:

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

`parameter-template.json` declares each pipeline input and
`mro2nf_overrides` (an `s3://` URI), all optional. A run can set nothing else,
so it cannot change the image or the inputs it may stage. A directory input is
an `s3://` prefix ending in `/` (`s3://<bucket>/fastqs/`): `start-run` checks
each URI and refuses a prefix without the slash with `S3 object not found`. Outputs are
published to `/mnt/workflow/pubdir`, which HealthOmics exports to the run's
output URI.

## A pipeline shipped as a bundle

A vendor bundle such as CellRanger runs its stages under its own environment
and writes the call it runs into a generated `.mro`. These steps ran
CellRanger 10.1.0 `count` on AWS Batch and HealthOmics:

1. Write the invocation without running it: `cellranger count --id=<id>
   --fastqs=<dir> --transcriptome=<dir> --dry` writes `__<id>.mro` into the
   working directory. The paths it names only need to exist for the dry run;
   the real inputs are run parameters.
2. Transpile it against the bundle's `.mro` tree:
   `mro2nf -mropath <bundle>/mro -o out __<id>.mro`.
3. Build the image `FROM` the runtime image. `COPY` the bundle and `__<id>.mro`
   to the absolute paths they had at transpile time, and set the environment
   the bundle's `sourceme.bash` sets as `ENV` lines (its `PATH` entries, ahead
   of `/opt/mro2nf`, `PYTHONPATH`, `MROPATH`, and the rest). `env -i bash -c
   'source <bundle>/sourceme.bash; env'` prints them.
4. Pass the inputs as a `-params-file` with the same typed values as the
   `.mro`'s call, the paths replaced by `s3://` URIs: a struct array such as
   CellRanger's `sample_def` goes in whole.

```dockerfile
FROM <runtime image>
COPY . /opt/cellranger-10.1.0/
COPY __pbmc.mro /work/pbmc/
ENV PATH=/opt/cellranger-10.1.0/lib/bin:/opt/cellranger-10.1.0/external/martian/bin:/opt/cellranger-10.1.0/external/anaconda/bin:/opt/cellranger-10.1.0/bin/tenkit:/opt/cellranger-10.1.0/bin:/opt/mro2nf:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    MROPATH=/opt/cellranger-10.1.0/mro \
    PYTHONPATH=/opt/cellranger-10.1.0/lib/python:/opt/cellranger-10.1.0/external/martian/adapters/python
```

CellRanger's stages write the FASTQ path they were given into JSON that
later stages open, so every task must see an input at the same path: the
`awsbatch` profile's mounted inputs and HealthOmics' shared run filesystem do,
and copy-staging executors do not.

## GPU stages

A stage requests GPUs with the reserved `special` resource:

```
stage ALIGN(
    in  fastq[] reads,
    out bam     aligned,
    src py      "stages/align",
) using (
    threads = 8,
    mem_gb  = 32,
    special = "gpu",      # one GPU; "gpu:4" for four
)
```

A task requests `accelerator N` when its job's `special`, as `mrp` resolves
it, is `gpu` or `gpu:N`. For a chunk or the join of a split stage, that is the
`__special` the split returned for it; the split job itself carries the
stage's own `special`. `special` also selects `params.job_resources[special]`
as the task's `clusterOptions`.

One rule in the generated `nextflow.config` sets `accelerator` per task. A
process `accelerator` directive that reads the task's inputs fails on Nextflow
25.10 and works on 26.04, so the request stays a config closure.

Executors read `accelerator` differently:

- On AWS Batch, Nextflow turns it into a GPU resource requirement on the job.
  One job queue can serve CPU and GPU stages: give it a CPU compute
  environment first and a GPU compute environment second. A GPU job cannot be
  placed on the CPU environment, so it goes to the GPU one. The GPU
  environment's instance types decide the GPU model and memory, and must
  include instances with at least as many GPUs as any stage requests.
- On HealthOmics, it requests a GPU instance. The account needs a service
  quota for GPU instance families.
- The local executor ignores it. On a grid, map the `special` to the
  scheduler's GPU option with `params.job_resources = [gpu: '--gres=gpu:1']`.

A container profile runs every task in one image, so a pipeline with a GPU
stage needs one CUDA image for all its stages. Copy mro2nf's runtime into it,
since the runtime image's base is `python:3.12-slim`:

```dockerfile
FROM ghcr.io/eunmann/mro2nf-runtime:<version> AS mro2nf
FROM nvidia/cuda:12.4.1-runtime-ubuntu22.04
# ps for Nextflow's task metrics, python3 for py stages, the aws CLI for Batch
RUN apt-get update \
 && apt-get install -y --no-install-recommends procps python3 awscli \
 && rm -rf /var/lib/apt/lists/*
COPY --from=mro2nf /opt/mro2nf /opt/mro2nf
ENV PATH="/opt/mro2nf:${PATH}"
COPY . /abs/path/the/pipeline/was/transpiled/from/
```

The host supplies the NVIDIA driver; the image needs the CUDA user-space
libraries and your GPU toolkit, at a CUDA version the driver supports.

## `mrp` flags in Nextflow

Settings marked `-c` go in a config file passed with `nextflow run -c <file>`.
HealthOmics takes no `-c`.

| `mrp` | mro2nf and Nextflow |
|---|---|
| pipestance name (second argument) | `--mro2nf_psid <id>` |
| `--jobmode=MODE` | `-profile <name>` |
| `--localcores=N`, `--localmem=GB` | `-c`: `executor { cpus = N; memory = '<GB> GB' }` together with `process.resourceLimits = [cpus: N, memory: <GB>.GB]`. Without the second line, Nextflow fails a task larger than the pool, where `mrp` clamps it. |
| `--maxjobs=N` | `-c`: `executor.queueSize = N` |
| `--jobinterval=MS` | `-c`: `executor.submitRateLimit = '<n>/1s'`, with `n` about `1000 / MS`, or `'1/<s>s'` for an interval of `s` whole seconds. Nextflow rejects an interval under one second. |
| `--overrides=JSON` | `--mro2nf_overrides <file>` |
| `--monitor` | `--mro2nf_monitor true` |
| `--autoretry=N` | `-c`: `process.maxRetries = N` |
| `--auto-adjust-memory` | not supported; [Retries](#retries) shows a `-c` config that raises memory on each retry |
| `--profile=MODE` | no stage profiling; the trace file the project enables records each task's `realtime`, `%cpu`, `peak_rss`, `peak_vmem`, `rchar` and `wchar`, and `-with-report` adds a report |
| `--onfinish=EXEC` | `-c`: a `workflow.onComplete` handler (below) |
| `--psdir=PATH` | the launch directory, `-work-dir` and `--outdir` |
| `--never-local` | the default: mro2nf schedules `local` stages like any other |
| `--vdrmode=MODE` | none: mro2nf deletes no intermediate file during a run; `nextflow clean` removes work directories afterwards |
| `--nopreflight` | none: preflight stages always run; to skip one, remove its call from the `.mro` and transpile again |
| `--inspect` | none; `nextflow log` lists past runs and their tasks |
| `--uiport`, `--disable-ui`, `--disable-auth`, `--require-auth`, `--auth-key`, `--https-cert`, `--https-key`, `--noexit` | no UI; `nextflow log`, `-with-report` and `-with-timeline` |
| `--zip` | none: `mrp` zips the pipestance's metadata files; each task's work directory keeps its jobs' metadata |
| `--localvmem`, `--mempercore`, `--limit-loadavg`, `--retry-wait`, `--tags`, `--strict`, `--stackvars`, `--debug`, `--stest` | none |
| `mro graph` | `nextflow run main.nf -with-dag dag.html` |
| `mrstat` | `nextflow log` |

`mrp` runs the `--onfinish` program with the pipestance path, `complete` or
`failed`, the pipestance ID, and the error file. A `-c` handler can pass the
output directory, the outcome and the ID:

```groovy
workflow.onComplete = {
    ['/path/to/hook', params.outdir, workflow.success ? 'complete' : 'failed', params.mro2nf_psid].execute().waitFor()
}
```

`nextflow lint` warns about a workflow handler in a config file; the run
executes it.

## Troubleshooting

### When a stage fails

Nextflow prints the failed process, its exit status, the error the stage or
`mre` wrote, and the task's work directory, then exits with status 1. A failed
run publishes nothing to `results/`.

| Task exit | Meaning | Retried |
|---|---|---|
| 42 | the stage failed an `ASSERT` (`martian.exit`) | no |
| 75 | a failure `mrp` retries: a signal, `ENOMEM`, a scheduler failure | up to twice |
| 137, 143 | the executor killed the task with a signal | up to twice |
| 1 | a stage exception, or a failed `mre` step | no |

A retried attempt that failed still counts as a failed task in the run's
summary, even when the run succeeds.

In the task's work directory:

- `.command.sh` is the task's script; `.command.err` and `.command.out` are its
  output.
- Each job `mrp` would run has a metadata directory: `split/`, `chnk<N>/` for
  a main (`chnk0/` for a stage that does not split) and `join/`. It holds
  `_args`, `_jobinfo`, `_outs`, `_stdout`, `_stderr`, `_log`, and `_errors`
  for an exception or `_assert` for an `ASSERT`.
- `journal/` holds the job journal files `mrjob` writes.

### Error messages

- **`the node compiles to another spec than the run's plan holds; re-transpile
  the pipeline, and rebuild its image, from the same sources`**: the `.mro`
  changed after the transpile, or the image holds other sources than the ones
  transpiled. Transpile again, and rebuild the image.
- **`mre: load call graph: read invocation: open <path>: no such file or
  directory`**: the task cannot see the pipeline's code at the path it was
  transpiled from. In a container, `COPY` the code to that path in the image.
- **`a container profile runs every task in params.mro2nf_image: pass
  --mro2nf_image <image>`**: a `docker`, `awsbatch` or `healthomics` run needs
  `--mro2nf_image`.
- **`entry input '<name>': the path contains '<c>', which Nextflow's task
  stage-in script leaves unquoted`**: Nextflow cannot stage a file whose name
  contains `*`, `?`, `[`, `]`, `{`, `}`, `$` or a control character. Rename
  the file.
- **`entry input '<name>': a head-node path is not staged on this profile;
  pass an s3:// URI`**: the `awsbatch` and `healthomics` profiles need file
  inputs on S3.
- **`entry input '<name>': scheme '<scheme>' is not allowed`**: pass a path or
  an `s3://` URI, or `--mro2nf_allow_remote_inputs true`.
- **`entry input '<name>': bucket <bucket> is not mounted at
  /mnt/s3/<bucket> on this run's instances; mount it there, or pass the
  input as an s3:// URI, which the tasks stage if their role may read the
  bucket`**: on `awsbatch`, the input is a path under `/mnt/s3` whose bucket
  is not mounted. Deploy the stack with the bucket in `mountBuckets`, or pass
  its `s3://` URI on a compute environment whose instance role may read the
  bucket: the example stack's may read only the buckets it mounts.
- **`entry input '<name>' names no object under the mounted bucket
  <bucket>`**: on `awsbatch`, no object or prefix has the input's key.
- **`entry override of a non-scalar type cannot be a string`**: pass the
  array, map or struct in a `-params-file`.
- **`Process requirement exceeds available memory`** (or `CPUs`): a task
  requests more than the local executor's pool. Set `process.resourceLimits`
  with `executor.cpus` and `executor.memory`, as in the table above.
