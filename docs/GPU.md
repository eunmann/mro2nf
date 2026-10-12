# GPU stages

A stage that needs a GPU declares it in the `.mro` with the reserved `special`
key. That is the only thing the pipeline author writes — no queue names, no
instance types, no cloud-specific config:

```
stage ALIGN(
    in  fastq[] reads,
    out bam     aligned,
    src py      "stages/align",
) using (
    special = "gpu",      # one GPU
)
```

For more than one GPU, give a count:

```
    special = "gpu:4",    # four GPUs
```

`special = "gpu"` is backend-neutral — it states a capability, not a backend. The
transpiler translates it per target. GPUs are whole devices, so the only thing you
can request is a **count**; there is no size/memory dimension (see "What does not
go in the .mro" below).

## What the transpiler emits

Each task requests `accelerator N` when the special of its job, as `mrp`
resolves it, is `gpu[:N]`: the stage's own `special`, or for a chunk or the
join of a split stage the `__special` the split returned for it, which
replaces the stage's. A split stage's split job carries the stage's own
special, as in `mrp`; a split that does no GPU work can leave `special` off
the stage and return `__special = "gpu"` for its chunks. One rule in the
generated `nextflow.config` evaluates this per task (a process `accelerator`
directive cannot read a task's inputs on Nextflow 25.10).

The `awsbatch` and `healthomics` profiles (`TRANSPILER.md` §9) run the setups
below. A GPU stage needs a CUDA image (below).

On the AWS Batch and HealthOmics executors, Nextflow turns `accelerator N` into a
GPU `resourceRequirement` on the job. That requirement is the routing signal the
backends use to place the job on a GPU instance.

The GPU count composes with the usual `threads`/`mem_gb` — a GPU job still
requests CPUs and RAM too:

```
using ( threads = 8, mem_gb = 32, special = "gpu" )
```

## The container

A container profile runs every task in the one image `--mro2nf_image` names
(README, "Container image"). So GPU support needs no per-stage container:
build that **one** image on a CUDA-capable base when the pipeline has any GPU
stage, with mro2nf's runtime copied from its image (`mre` and `mrjob` are
static) onto the `PATH`, where every task finds `mre`, which runs the `mrjob`
beside it, and the pipeline's code at the paths it was transpiled from. It
copies the runtime rather than building `FROM` the runtime image, whose base
is `python:3.12-slim`, not CUDA.

```dockerfile
FROM ghcr.io/eunmann/mro2nf-runtime:<version> AS mro2nf
FROM nvidia/cuda:12.4.1-runtime-ubuntu22.04
# bash, ps (Nextflow task metrics), python3 for py stages, the aws CLI for Batch
RUN apt-get update \
 && apt-get install -y --no-install-recommends procps python3 awscli \
 && rm -rf /var/lib/apt/lists/*
COPY --from=mro2nf /opt/mro2nf /opt/mro2nf
ENV PATH="/opt/mro2nf:${PATH}"
COPY my-pipeline/ /opt/pipeline/
```

CPU stages run in the same image and simply never touch the GPU. The NVIDIA
driver comes from the host (the GPU AMI on Batch, the managed host on
HealthOmics); the image only needs the CUDA **userspace** libraries, plus
whatever GPU toolkit your stage code uses (PyTorch, etc.). Match the CUDA version
to what the host driver supports.

## AWS Batch setup

You do not name a queue per stage. Use **one** job queue that fronts two compute
environments, in this order:

1. **order 1 — CPU compute environment** (general instance types, no GPU)
2. **order 2 — GPU compute environment** (GPU instance types: `g5`, `g4dn`,
   `p4d`, …, which Batch runs on the ECS GPU-optimized AMI automatically)

AWS Batch is GPU-aware: a job with a GPU `resourceRequirement` cannot be placed on
the CPU-only environment, so Batch falls through to the GPU environment. A job
without one lands on the cheaper CPU environment first. The result:

- normal stages → CPU compute environment,
- `special = "gpu"` stages → GPU compute environment,

…through the **same queue**, chosen only at launch:

```bash
nextflow run main.nf -profile awsbatch -c batch.config \
    --mro2nf_image <your-cuda-image> -work-dir s3://<bucket>/work
```

where `batch.config` sets `process.queue = '<the-one-queue>'` and
`aws.region = '<r>'`.

Two things to keep in mind:

- **Count vs instance size.** A `special = "gpu:4"` stage only fits instances with
  at least four GPUs, so the GPU compute environment must offer such instance
  types or the job waits forever in `RUNNABLE`. This is the same coupling as a
  large `mem_gb` needing a large instance.
- **CPU overflow.** With a single queue, if the CPU environment is saturated
  (at its `maxvCpus`), Batch can spill plain CPU jobs onto GPU instances (a GPU
  box runs CPU work, just expensively). It will not happen at normal load. If you
  need a hard guarantee, use two separate queues instead — but then the GPU stages
  must target the GPU queue, which reintroduces queue knowledge.

## AWS HealthOmics setup

Simpler — HealthOmics has no queues. The `accelerator N` directive routes the task
to a GPU-backed instance, managed by the service. You only need:

- the CUDA image in a **private ECR** repo in the run's region, pinned by
  digest in the package (`./package.sh <image>@sha256:…`), and
- (if applicable) a HealthOmics service quota that permits GPU instance families.

Everything else (packaging, `parameter-template.json`) is unchanged.

## Other executors

The same `special = "gpu"` works elsewhere via the `accelerator` directive, which
each executor interprets: Kubernetes and Google Batch consume it directly; the
local executor treats it as a no-op hint (handy for a CPU dry run). For HPC grid
schedulers (SLURM/SGE/LSF), GPU requests usually go through `clusterOptions`;
every task also looks its special up in `params.job_resources`, the
`MRO_JOBRESOURCES` analog, so `params.job_resources = [gpu: '--gres=gpu:1']` in
a `-c` config routes a `special = "gpu"` job to a GPU partition.

## What does not go in the .mro

By design, the `.mro` carries only the GPU **count**. Everything else is infra,
decided at deploy or launch — never in the pipeline:

- **GPU type / memory** (T4 vs A100, 16 GB vs 80 GB) is a property of the compute
  environment's instance types. To use a particular GPU class, deploy a GPU
  compute environment built from those instances. There is no Batch request field
  for GPU memory, so there is nothing to put in the `.mro`.
- **The queue** is a launch setting (`process.queue`), and routing is handled
  by the ordered compute environments above.
- **The image** is a launch argument (`--mro2nf_image`), or pinned in the
  HealthOmics package.

The pipeline says *how many* GPUs each stage needs; the deployment decides *what
kind* and *where they run*.
