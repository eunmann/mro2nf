# Using mro2nf

Install mro2nf, transpile a Martian pipeline, and run it with Nextflow.

## Requirements

| Need | For |
|---|---|
| Linux, amd64 or arm64 | everything: mro2nf ships and is tested on Linux only |
| Nextflow 26.04.0 or later, on Java 17 to 26 | running a project (`curl -s https://get.nextflow.io \| bash`) |
| Python 3 | `py` stages on the `standard` and grid profiles |
| Docker | the `docker` profile, and building images |
| `zip` | `package.sh`, which packages a HealthOmics workflow |
| Go 1.27 and `make` | building from source |

A project declares `manifest.nextflowVersion '!>=26.04.0'`, so an older
Nextflow refuses to run it. It needs Nextflow's default strict (v2) syntax
parser: under `NXF_SYNTAX_PARSER=v1`, Nextflow converts command-line values
before the run sees them (`--s 007` becomes the number `7`).

## Install

### From a release

Each [release](https://github.com/eunmann/mro2nf/releases) has an archive per
platform and a `SHA256SUMS` file:

```bash
tag=v1.2.3 platform=linux-amd64       # or linux-arm64
base=https://github.com/eunmann/mro2nf/releases/download/$tag
curl -fL -O "$base/mro2nf-$tag-$platform.tar.gz" -O "$base/SHA256SUMS"
sha256sum -c SHA256SUMS --ignore-missing
tar -xzf "mro2nf-$tag-$platform.tar.gz"
export PATH="$PWD/mro2nf-$tag-$platform:$PATH"
mro2nf -version
```

The archive holds three programs:

| Program | Role |
|---|---|
| `mro2nf` | the transpiler |
| `mre` | the runtime shim every task of a run calls |
| `mrjob` | Martian's job runner, which `mre` runs for `py` and `comp` stages |

Keep `mre` and `mrjob` in one directory: `mre` runs the `mrjob` beside it.
Releases after v0.6.0 carry build provenance, which
`gh attestation verify <archive> --repo eunmann/mro2nf` checks.

### From source

```bash
git clone https://github.com/eunmann/mro2nf
cd mro2nf
make install                          # into ~/.local/bin; PREFIX=<dir> for <dir>/bin
export PATH="$HOME/.local/bin:$PATH"
```

## Transpile

```bash
mro2nf [-o <dir>] [-mropath <path>] <pipeline.mro>
```

| Argument | Meaning |
|---|---|
| `<pipeline.mro>` | the entry file: the pipeline and the `call` `mrp` would run. The call's arguments are the run's default inputs. |
| `-o <dir>` | the project directory (default `out`) |
| `-mropath <path>` | the `@include` search path, `:`-separated. Default: `$MROPATH`, else the entry's directory, as under `mrp`. |
| `-version` | print the version |

mro2nf compiles the pipeline with Martian's compiler, so it fails with
`mrp`'s message wherever `mrp` would refuse to start, and writes no project.
It also refuses:

- a shape whose data the tasks cannot exchange
  ([Feature coverage](feature-coverage.md));
- an entry input named `job_resources` or `mro2nf_*`, which the project
  defines itself;
- an entry input named with a Groovy or Nextflow keyword, such as `def` or
  `class`, which Nextflow's `params` block cannot declare (divergence
  `keyword-input`);
- a number literal `mrp` panics on: an int outside int64, a float beyond
  float64, or a `threads`, `mem_gb` or `vmem_gb` beyond float32.

> [!IMPORTANT]
> The project names the entry `.mro`, its includes and its stage code by
> their absolute paths at transpile time, and every profile runs the code
> from there. Don't move them; if you do, transpile again.

### The generated project

| File | Holds |
|---|---|
| `main.nf` | the parameters, PLAN, the stage calls, OUTS and the published outputs |
| `subworkflows/` | in a pipeline too large for one workflow, one workflow per run of stage calls |
| `stage.nf` | the stage processes and the wiring every stage call uses |
| `nextflow.config`, `conf/` | the Nextflow floor, the settings every profile shares, the profiles |
| `nextflow_schema.json` | every parameter's type, default and description, in the nf-schema format |
| `package.sh`, `parameter-template.json` | the HealthOmics package |
| `_assets/` | a copy of the `.mro` files, which PLAN and OUTS compile |

Each stage call is a process named `N<i>_<CALL>`. A split stage has three:
`N<i>_<CALL>_SP`, `_MN` and `_JN`, for its split, chunk (main) and join
phases. Use these names in `withName:` selectors. The trace shows a process
inside a sub-workflow as `W<j>:N<i>_<CALL>`; a selector of `N<i>_<CALL>`
matches it.

## Run

### A first run

This example transpiles a fixture of this repository, so run it from a clone:

```bash
mro2nf -o out testdata/split_test/pipeline.mro
cd out
echo '{"values": [5, 6, 7]}' > inputs.json
nextflow run main.nf -params-file inputs.json
cat results/pipeline_outs.json        # {"sum": 110}
```

The run has 8 tasks: PLAN, the split of `SUM_SQUARES`, one task per chunk
(3), its join, `REPORT`, and OUTS. `mrp` runs the same 6 stage jobs.

### Outputs

| Path | Holds |
|---|---|
| `results/` | `mrp`'s `outs/` tree and `pipeline_outs.json` |
| `work/` | one directory per task, with the job's metadata and logs |
| `.nextflow/`, `.nextflow.log`, `trace-<timestamp>.txt` | Nextflow's state, log and task trace |

A failed run publishes nothing. `-output-dir <dir>`, or `outputDir = '<dir>'`
in a `-c` config, publishes elsewhere, an `s3://` prefix included.

### Resume

`nextflow run main.nf -params-file inputs.json -resume` reuses every task
whose inputs did not change.

- A changed entry value re-runs every task: every task reads the run's call.
- A changed `--mro2nf_psid` re-runs every task; `--mro2nf_monitor`, every
  stage task and OUTS; an overrides file, the stages whose resources or
  profiles it changes and the tasks after them.
- A `-c` change to `cpus` or `memory` re-runs nothing.
- An input read in place is keyed on its path; a staged input on its path,
  size and modification time (a directory's own, not its files'). Give
  changed data a new path, or run without `-resume`.

> [!WARNING]
> Nextflow does not see edits to stage code. After changing stage code, run
> without `-resume`.

## Profiles

One project runs on every target; `-profile <name>` picks it.

| Profile | Runs on | Needs |
|---|---|---|
| `standard` (default) | the local host | `mre`, `mrjob`, Python 3 and the pipeline's code on the host |
| `slurm`, `sge`, `lsf`, `pbs` | a grid scheduler | the same, on a filesystem the compute nodes share |
| `docker` | local Docker, a container per task | `--mro2nf_image` |
| `awsbatch` | AWS Batch over an S3 work directory | `--mro2nf_image` in ECR, `-work-dir s3://…`, `process.queue`, `aws.region` |
| `healthomics` | AWS HealthOmics | `./package.sh <image>@sha256:<digest>` |

[`deploy/awsbatch-cdk`](../deploy/awsbatch-cdk/README.md) deploys the AWS
resources both AWS profiles need, and walks through a run on each.

### Local and grid

These profiles run the host's `mre` and the pipeline's code in place, and read
absolute file inputs where they are. A grid profile sets only
`process.executor`; put the queue and other scheduler settings in a `-c`
config. `params.job_resources` maps a stage's `special` value to the task's
`clusterOptions` ([Parameters](#job_resources)).

### Docker

```bash
nextflow run main.nf -profile docker --mro2nf_image <image>
```

Each container runs as the invoking user with `HOME=/tmp`
(`process.containerOptions = '-u $(id -u):$(id -g) -e HOME=/tmp'`), so
`work/` stays yours. A `docker.runOptions` in a `-c` config adds to that; a
`process.containerOptions` replaces it. On rootless Docker set
`process.containerOptions = ''`: the container's root already is you.

### AWS Batch

Push the pipeline's image to ECR, built for the compute environment's
architecture. Name the queue, region and the tasks' IAM role (otherwise the
instance's) in a config:

```groovy
// batch.config
process.queue = '<job queue>'
aws.region = '<region>'
aws.batch.jobRole = '<job role ARN>'
```

```bash
nextflow run main.nf -profile awsbatch -c batch.config \
    --mro2nf_image <account>.dkr.ecr.<region>.amazonaws.com/<repo>:<tag> \
    -work-dir s3://<bucket>/work -output-dir s3://<bucket>/results
```

- Pass file inputs as `s3://` URIs; the profile refuses head-node paths.
- Keep `-output-dir` on S3: the default `results` publishes to the launch
  host.
- The profile limits submission and S3 transfers so a wide fan-out does not
  throttle AWS, retries a reclaimed Spot task, and polls job status every 2 s.

#### Reading S3 inputs in place

Where the instances mount an input's bucket read-only at `/mnt/s3/<bucket>`
(the example stack does, with Mountpoint for S3), the profile bind-mounts
`/mnt/s3` into every container and passes the stage
`/mnt/s3/<bucket>/<key>` instead of staging the file. Every task then sees one
path, as under `mrp`, so a stage may record it for a later stage to open.

- PLAN decides each input at run start: a mounted bucket is read in place,
  any other is staged. Every instance must mount the same buckets.
- A staged input's bucket must be readable by the tasks' role.
- A key no object has fails the run at its start. A key with an empty, `.`
  or `..` component is refused, as Mountpoint cannot show it.
- `-resume` keys a mounted input on its path: put new data under a new key.
- `--mro2nf_s3_mount <dir>` names another root.

### AWS HealthOmics

HealthOmics takes no `-c` and applies `standard`, the local profile, to a run
that names none. So `package.sh` builds a workflow whose own config is
`conf/base.config`, the `healthomics` profile's settings and the image pinned
by digest, with no profiles and no executor:

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

- `parameter-template.json` declares the pipeline inputs and
  `mro2nf_overrides`, all optional; a run can set nothing else, so it cannot
  change the image.
- Settings added to `nextflow.config` stay out of the package; put them in
  `conf/base.config` or `conf/healthomics.config`.
- A directory input is an `s3://` prefix ending in `/`.
- Outputs land at `<output-uri>/<run-id>/`: `pipeline_outs.json` and `mrp`'s
  `outs/` tree.
- HealthOmics grants whole GiB, so a task's memory is its request rounded up;
  the stage still sees its request in `_jobinfo`.

## Parameters

### Pipeline inputs

Each entry input is a parameter of the same name. Unset, it takes the value
in the `.mro`'s `call`. PLAN compiles the call with the launch values, so a
value changes forks, disabled calls and chunk counts as the same literal in
the `.mro` would under `mrp`.

| Value | How | Example |
|---|---|---|
| `int`, `float`, `bool`, `string` | command line or `-params-file` | `--n 5 --skip false` |
| array, map, struct | typed JSON or YAML in a `-params-file` | `{"values": [5, 6, 7]}` |
| `null` | a `-params-file` (`--n null` is the string `null`) | `{"n": null}` |
| an input the top-level `map call` splits | Martian's split form in a `-params-file` | `{"x": {"split": [4, 5]}}` |
| `file`, `path`, a `filetype` | an absolute path or an `s3://` URI | `--reads /data/r1.fastq` |
| an input named `_x` | `--_x=value` | `--_tag=x` |

- An undeclared parameter, such as a typo, fails the launch before any task.
- PLAN checks every value against its Martian type and fails naming the
  input, its type and the value.
- A whole-number float for an `int` (`{"n": 5.0}`) runs as the int, as
  `mrp` reads the literal `5.0`.

> [!CAUTION]
> Nextflow's JSON reader wraps an int beyond int64 (`9223372036854775808`
> becomes `-9223372036854775808`) and reads `-0.0` as `0.0` before mro2nf
> sees them (divergence `params-json-int-wrap`). A YAML params file or the
> command line keeps both.

`nextflow_schema.json` describes every parameter for launch forms. Nothing
loads it at launch; PLAN does the type checks.

### mro2nf options

| Parameter | Default | Effect |
|---|---|---|
| `--mro2nf_overrides <file>` | none | `mrp`'s `--overrides` file ([below](#mrp---overrides)) |
| `--mro2nf_monitor true` | `false` | `mrp`'s `--monitor`: `mrjob` kills a job over its memory request |
| `--mro2nf_confine true` | `false` | fail a task whose output resolves outside its work directory, instead of copying it in; the container profiles always confine |
| `--mro2nf_image <image>` | none | the image every task runs in; the container profiles require it |
| `--mro2nf_mre <path>` | `mre` on `PATH` | the `mre` the tasks run |
| `--mro2nf_mrjob <path>` | the `mrjob` beside `mre` | the `mrjob` for `py` and `comp` stages |
| `--mro2nf_adapters <dir>` | the adapters `mre` embeds | Martian's adapters, holding `python/martian_shell.py` |
| `--mro2nf_psid <id>` | the call's name | `mrp`'s pipestance ID, which names each job in `_jobinfo` |
| `--mro2nf_allow_remote_inputs true` | `false` | accept file inputs with schemes other than `s3://` |
| `--mro2nf_s3_mount <dir>` | `/mnt/s3` on `awsbatch` | the root the instances mount input buckets under |

`--mro2nf_mre`, `--mro2nf_mrjob` and `--mro2nf_adapters` run the stages under
another Martian runtime, such as a vendor bundle's. A boolean option alone
(`--mro2nf_monitor`) means `true`.

### job_resources

`params.job_resources`, set in a `-c` config, maps a stage's `special` value to
its tasks' `clusterOptions` on a grid, like `mrp`'s `MRO_JOBRESOURCES`:

```groovy
params.job_resources = [highmem: '--partition=highmem']
```

## Resources and retries

### How a job is sized

Each job is granted its request as `mrp` grants it in the jobmode the profile
runs as. The task requests the grant (`cpus` rounded up, `memory`), and the
stage sees it in `_jobinfo`, `__threads`, `__mem_gb`, `__vmem_gb` and `mrp`'s
thread variables such as `OMP_NUM_THREADS`.

| Profiles | Granted as `mrp`'s | Rule |
|---|---|---|
| `standard`, `docker` | local jobmode | from a pool, by default the host's cpus and 90% of its memory; an adaptive (negative) request gets the whole pool, and every request but `vmem_gb` is capped at it |
| grid, `awsbatch`, `healthomics` | cluster jobmode | a negative request gets its magnitude, threads round up, nothing is capped |

Unset, a job gets 1 thread, 1 GB, and `vmem_gb` 3 GB above its memory. A
split's chunks and join get the resources the split returned for them. To
grant from another pool, as `mrp --localcores=8 --localmem=32` does:

```groovy
process.resourceLimits = [cpus: 8, memory: 32.GB]
```

Under a cgroup quota, Nextflow counts the quota's cpus where `mrp` counts every
cpu, so set the pool to match `mrp`'s. An adaptive memory request is the one
grant that can differ: `mrp` grants what the host has free (divergence
`adaptive-memory-pool`).

### mrp --overrides

```bash
nextflow run main.nf --mro2nf_overrides overrides.json
```

PLAN reads the file with Martian's reader, so a key `mrp` does not know fails
the run before any stage. Each job's resources resolve as under `mrp`, into its
task and what the stage sees. A `*.profile` key profiles the job as under
`mrp`: the stage's adapter profiles `cpu`, `line` and `mem`, and `mrjob` runs
`perf`, `pyspy` and `pyflame` from the task's `PATH`, logging and running the
job unprofiled without it. The profile lands in the job's metadata directory
([A task's work directory](#a-tasks-work-directory)). `force_volatile` has no
effect.

### Size one stage

A `withName:` selector sizes a stage's tasks without changing what the stage
sees:

```groovy
process {
    withName: 'N3_ALIGN_MN' { memory = '8 GB'; cpus = 4 }
}
```

### Retries

A task is retried only for a failure that can pass on a second attempt, up to
twice (`mrp`'s `default_retries`), at the same cpus and memory:

| Exit | Meaning | Retried |
|---|---|---|
| 42 | the stage failed an `ASSERT` (`martian.exit`) | no |
| 75 | a failure `mrp` retries: an `_errors` line matches a `retry_on` pattern of `mrp`'s `jobmanagers/retry.json` (a signal, `ENOMEM`, a scheduler failure) | yes |
| 104, 130–145 | the executor killed the task or stopped it at a time limit (137: out of memory) | yes |
| 1 | a stage exception, or a failed `mre` step | no |

`process.maxRetries = N` in a `-c` config is `mrp --autoretry=N`. mro2nf has
no `--auto-adjust-memory`; this config raises memory on each retry instead:

```groovy
process {
    withName: 'N[0-9]+_.*' { memory = { "${meta.mem_gb * task.attempt} GB" } }
}
```

### GPU stages

A stage whose `special`, as `mrp` resolves it, is `gpu` or `gpu:N` requests 1
or N GPUs (`accelerator`). For a split's chunks and join that is the
`__special` the split returned.

- **AWS Batch** adds a GPU requirement to the job. One queue can serve both
  kinds: a CPU compute environment first, a GPU one second.
- **HealthOmics** requests a GPU instance; the account needs the GPU quota.
- **Local** ignores it. On a **grid**, map it:
  `params.job_resources = [gpu: '--gres=gpu:1']`.

A container profile runs every task in one image, so a pipeline with a GPU
stage needs one CUDA image ([below](#a-cuda-image)).

## Container images

The `docker`, `awsbatch` and `healthomics` profiles run every task in the one
image `--mro2nf_image` names. Build it `FROM` the mro2nf runtime image and add
the pipeline's code.

### The runtime image

`ghcr.io/eunmann/mro2nf-runtime:<tag>` holds `mre`, `mrjob` and the Martian
adapters at `/opt/mro2nf`, on `PATH`, on `python:3.12-slim`, for linux/amd64
and linux/arm64. Use the tag of the release that transpiled the project: PLAN
fails under another build's `mre` with
`project transpiled by <build>, mre is <build>`.

For an unreleased build, build the image from the same checkout, with the
build your `mro2nf` names:

```bash
docker build --build-arg VERSION="$(mro2nf -version | cut -d' ' -f2)" \
  -f deploy/runtime/Dockerfile -t mro2nf-runtime:local .
```

### Your pipeline's image

Copy the code, the entry `.mro` included, to the absolute path it was
transpiled from. For the [first run](#a-first-run), from the clone's root:

```bash
cat > split_test.Dockerfile <<EOF
FROM mro2nf-runtime:local
COPY . $PWD/testdata/split_test/
EOF
docker build -f split_test.Dockerfile -t split-test:local testdata/split_test
cd out
nextflow run main.nf -profile docker --mro2nf_image split-test:local -params-file inputs.json
```

Install what the stages need in the same image: Python packages, and the
program a stage whose `src` names a command runs. A task whose code compiles to
another pipeline than PLAN planned fails with `the node compiles to another
spec than the run's plan holds`.

### A vendor bundle such as Cell Ranger

A bundle runs its stages under its own environment and writes the call it
runs into a generated `.mro`. These steps ran Cell Ranger 10.1.0 `count`:

1. `cellranger count --id=<id> --fastqs=<dir> --transcriptome=<dir> --dry`
   writes `__<id>.mro`.
2. `mro2nf -mropath <bundle>/mro -o out __<id>.mro`.
3. Build the image `FROM` the runtime image: `COPY` the bundle and
   `__<id>.mro` to their transpile-time paths, and set the environment the
   bundle's `sourceme.bash` sets as `ENV` lines, its `PATH` entries ahead of
   `/opt/mro2nf` (`env -i bash -c 'source <bundle>/sourceme.bash; env'`
   prints them).
4. Pass the inputs in a `-params-file`, with `s3://` URIs for paths.

Cell Ranger writes its FASTQ path into JSON that later stages open, so on
`awsbatch` the FASTQ bucket must be [mounted](#reading-s3-inputs-in-place).

### A CUDA image

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

The host supplies the NVIDIA driver.

## Files and paths

`mrp` runs every job on one filesystem. A Nextflow task sees only what is
staged into it, so mro2nf stages each file to the tasks whose `_args` name it.

### File inputs

A file input is an absolute path or an `s3://` URI. Other schemes need
`--mro2nf_allow_remote_inputs true`.

| Input | `standard`, grid | `docker` | `awsbatch` | `healthomics` |
|---|---|---|---|---|
| absolute path | read in place | staged | refused, unless under `/mnt/s3` | refused |
| `s3://` URI | staged | staged | read in place if its bucket is mounted, else staged | staged |

A stage given a path read in place sees that path, as under `mrp`. A stage
given a staged file sees a path in its own task directory with the same name
and content (divergence `staged-input-paths`). On the local and grid profiles,
an input that is or holds the run's work directory is refused.

### Companion files

A task stages only the files an input names. A stage that opens a file beside
its input, such as an index beside a BAM, finds it under `mrp` but not when
the input is another stage's output or a staged input (divergence
`input-file-siblings`). Declare the companion as its own input, and as its own
output of the stage that writes it, or pass a directory holding both.

### Paths written into text

Typed file values (`file`, `path`, a filetype, and every file leaf of a struct,
array or map whose type declares it) reach every task that reads them, on
every profile. A path a stage writes into text, such as JSON, a CSV, an untyped
`map` or a `string`, is not staged: it opens only where the later task sees
the same path. Give the file a file type instead.

| Profile | input path in text | stage output path in text |
|---|---|---|
| `standard`, grid | opens | opens |
| `docker` | opens for a remote input, fails for a head-node input | opens |
| `awsbatch` | opens under the S3 mount, fails otherwise | fails |
| `healthomics` | opens | opens |

Cell Ranger `count` writes its FASTQ path into JSON; `multi` and `aggr` take a
CSV of paths. They run on `standard`, grid, `healthomics`, and on `awsbatch`
with their inputs under the S3 mount.

### File names Nextflow cannot stage

Nextflow's stage-in script leaves `*`, `?`, `[`, `]`, `{`, `}`, `$` and control
characters unquoted. PLAN refuses a staged input whose path holds one. Rename
the file, or on `standard` and grid pass an absolute path, which is read in
place and never staged.

## mrp equivalents

Settings marked `-c` go in a config passed with `nextflow run -c <file>`.

| `mrp` | mro2nf and Nextflow |
|---|---|
| pipestance name | `--mro2nf_psid <id>` |
| `--jobmode` | `-profile` |
| `--localcores`, `--localmem` | `-c`: `process.resourceLimits = [cpus: N, memory: <GB>.GB]` |
| `--maxjobs=N` | `-c`: `executor.queueSize = N` |
| `--jobinterval` | `-c`: `executor.submitRateLimit = '<n>/1s'` (one second at least) |
| `--overrides` | `--mro2nf_overrides <file>` |
| `--monitor` | `--mro2nf_monitor true` |
| `--autoretry=N` | `-c`: `process.maxRetries = N` |
| `--psdir` | the launch directory, `-work-dir`, `-output-dir` |
| `--onfinish` | `-c`: a `workflow.onComplete` handler |
| `--profile`, `--inspect`, the UI, `mrstat` | the trace file, `-with-report`, `-with-timeline`, `nextflow log` |
| `mro graph` | `nextflow run main.nf -with-dag dag.html` |
| `MRO_FORCE_UUID` | the same, in `nextflow run`'s environment; unset, the uuid is the Nextflow session id, which `-resume` keeps |
| `--vdrmode`, `--zip`, `--strict`, `--auto-adjust-memory`, `--nopreflight` | none |

## Troubleshooting

Nextflow prints the failed process, its exit status ([Retries](#retries)),
the error the stage or `mre` wrote, and the task's work directory, and exits 1.

### A task's work directory

```
work/ab/cdef…/
├── .command.sh, .command.out, .command.err   the task's script, stdout, stderr
├── chnk0/      the job's metadata: split/, chnk<N>/ or join/; chnk0/ for a
│               stage that does not split
└── journal/    mrjob's journal
```

The metadata directory holds what `mrp` writes for the job: `_args`,
`_jobinfo`, `_outs`, `_stdout`, `_stderr`, `_log`, and `_errors` or `_assert`.

On `awsbatch` the work directory is on S3, and Nextflow uploads a task's
outputs only when it succeeds. A failed task keeps `.command.sh`,
`.command.run`, `.command.out`, `.command.err`, `.command.log` (also the job's
CloudWatch log stream), `.command.trace` and `.exitcode`. `mre` copies the
job's stdout and stderr, and the text of `_errors` or `_assert`, into the
task's. To read `_args`, `_jobinfo` or `_outs`, re-run the inputs on the
`docker` profile.

### Error messages

| Message | Fix |
|---|---|
| `project transpiled by <build>, mre is <build>` | run the `mre` of the build that transpiled the project (`PATH`, `--mro2nf_mre`, or that release's runtime image), or transpile again |
| `the node compiles to another spec than the run's plan holds` | the `.mro` or the image's code changed since the transpile: transpile again and rebuild the image |
| `load call graph: read invocation: open <path>: no such file or directory` | the task cannot see the code at its transpile-time path: `COPY` it there in the image |
| `entry override of a non-scalar type cannot be a string` | pass the array, map or struct in a `-params-file` |
| `entry override of a split input must be {"split": [...]} or {"split": {...}}` | pass the split input in Martian's split form |
| `a head-node path is not staged on this profile; pass an s3:// URI` | `awsbatch` and `healthomics` need inputs on S3 |
| `is or holds the run's work dir` | pass a directory outside the work directory, or move `-work-dir` |
| `scheme '<scheme>' is not allowed` | pass a path or `s3://` URI, or `--mro2nf_allow_remote_inputs true` |
| `bucket <bucket> is not mounted at /mnt/s3/<bucket>` | add the bucket to the stack's `mountBuckets`, or pass its `s3://` URI |
| `a container profile runs every task in params.mro2nf_image` | pass `--mro2nf_image` |
| `Process requirement exceeds available memory` (or `CPUs`) | set `process.resourceLimits` no larger than the executor's `cpus` and `memory` |

## Upgrading

1. Install the new release, or `make install` from the new checkout.
2. Transpile the pipeline again. PLAN runs a project only under the `mre` of
   the build that transpiled it.
3. On a container profile, rebuild the pipeline's image `FROM` the new
   runtime image tag. On HealthOmics, run `package.sh` again and register a
   new workflow version.
4. The first `-resume` after an upgrade re-runs every task: each task's cache
   key holds the mro2nf build. Later resumes cache as before.

The [release notes](https://github.com/eunmann/mro2nf/releases) list each
release's changes. From v0.5.0 or earlier, HealthOmics outputs moved from
`<output-uri>/<run-id>/pubdir/` to `<output-uri>/<run-id>/`.

## Compatibility

| Component | Supported and tested |
|---|---|
| Martian | `github.com/martian-lang/martian` at commit `50ad6c054d30` (pinned in `go.mod`); the differential tests run `mrp` built from the same commit |
| Nextflow | 26.04.0 or later. PR Validation runs the latest stable release; the nightly runs 26.04.0 and the latest stable |
| Java | 17 to 26 (Nextflow 26.04's range). CI runs Temurin 25, and 17 on the nightly's 26.04.0 lane |
| OS | Linux, amd64 and arm64 (release archives and runtime image) |
| Python | 3.12 in the runtime image |

Live runs on AWS, last at build `e1afb3d` (2026-10-12 to 2026-10-14), with
Nextflow 26.04.7 on the launch host:

| Target | What ran | Result |
|---|---|---|
| AWS Batch | 40 fixtures, `results/` compared with a local run of each | 36 identical; 4 differ as expected (2 meant to fail, 1 too large for the queue, 1 granted by cluster rules) |
| AWS Batch | `-resume` over S3 (4 fixtures); `-output-dir s3://…` | every stage task cached; published tree identical |
| AWS Batch | Cell Ranger 10.1.0 `count` (pbmc_1k_v3), 4 runs | 153 tasks each; `metrics_summary.csv` identical across runs |
| HealthOmics | 9 fixtures on engine 25.10.0, 4 on 26.04.0 | identical to local runs |
