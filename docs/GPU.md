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

For a GPU stage, `mro2nf` emits an `accelerator N` directive on the **compute
phase only**:

- a non-split stage → its single process,
- a split stage → its `MAIN` phase (not `SPLIT` or `JOIN`, which do no GPU work).

On the AWS Batch and HealthOmics executors, Nextflow turns `accelerator N` into a
GPU `resourceRequirement` on the job. That requirement is the routing signal the
backends use to place the job on a GPU instance.

The GPU count composes with the usual `threads`/`mem_gb` — a GPU job still
requests CPUs and RAM too:

```
using ( threads = 8, mem_gb = 32, special = "gpu" )
```

## The container

The transpiler bakes every stage into one image and runs all stages from it. So
GPU support needs no per-stage container: build that **one** image on a
CUDA-capable base when the pipeline has any GPU stage.

```dockerfile
FROM nvidia/cuda:12.4.1-runtime-ubuntu22.04
# install python3 + the aws CLI, then add the runtime the usual way:
COPY mre /opt/mro2nf/mre
COPY adapters /opt/mro2nf/adapters
COPY stages /opt/mro2nf/stages
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
nextflow run main.nf --aws_queue <the-one-queue> --aws_region <r> \
    --container <your-cuda-ecr-uri> -work-dir s3://<bucket>/work
```

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

- the CUDA image in a **private ECR** repo in the run's region, passed as the
  `container` run parameter, and
- (if applicable) a HealthOmics service quota that permits GPU instance families.

Everything else (packaging, `parameter-template.json`) is unchanged.

## Other executors

The same `special = "gpu"` works elsewhere via the `accelerator` directive, which
each executor interprets: Kubernetes and Google Batch consume it directly; the
local executor treats it as a no-op hint (handy for a CPU dry run). For HPC grid
schedulers (SLURM/SGE/LSF), GPU requests usually go through `clusterOptions`
(`--gres=gpu:N`) rather than `accelerator`; if you target a grid GPU partition,
add that via a `-c` config overlay for now.

## What does not go in the .mro

By design, the `.mro` carries only the GPU **count**. Everything else is infra,
decided at deploy or launch — never in the pipeline:

- **GPU type / memory** (T4 vs A100, 16 GB vs 80 GB) is a property of the compute
  environment's instance types. To use a particular GPU class, deploy a GPU
  compute environment built from those instances. There is no Batch request field
  for GPU memory, so there is nothing to put in the `.mro`.
- **The queue** is a launch argument (`--aws_queue`), and routing is handled by the
  ordered compute environments above.
- **The image** is a launch argument (`--container`).

The pipeline says *how many* GPUs each stage needs; the deployment decides *what
kind* and *where they run*.
