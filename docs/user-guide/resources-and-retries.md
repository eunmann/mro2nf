# Resources and retries

## How a job is sized

Each job is granted its request as `mrp` grants it in the jobmode the profile
runs as, and its task is sized to that grant.

| Profiles | Runs as `mrp`'s | Grant |
|---|---|---|
| `standard`, `docker` | local jobmode | from a pool: the cpus and whole GB of memory `process.resourceLimits` sets, by default the host's cpus and 90% of its memory. A negative (adaptive) request gets the whole pool, and every request but `vmem_gb` is capped at the pool. Fractions are kept, to the hundredth of a thread and the MiB. |
| `slurm`, `sge`, `lsf`, `pbs`, `awsbatch`, `healthomics`, the HealthOmics package | cluster jobmode | a negative request gets its magnitude, threads round up to whole threads, and nothing is capped |

On every profile:

- Unset, a job gets 1 thread and 1 GB, and `vmem_gb` the memory plus 3 GB.
- A chunk and the join use the `__threads`, `__mem_gb`, `__vmem_gb` and
  `__special` that the split returned for them.
- The stage sees its grant in `_jobinfo`, in `__threads`, `__mem_gb` and
  `__vmem_gb`, and in `mrp`'s thread variables, such as `OMP_NUM_THREADS`, set
  to its threads rounded up.
- The task's `cpus` is the job's threads, rounded up, and its `memory` the
  job's `mem_gb`.

The default pool is `mrp`'s default `--localcores` and `--localmem` on a host
without a cgroup cpu quota or memory limit. Under one, Nextflow counts the
quota's cpus where `mrp` counts every cpu, and `mrp` may take 96% of the
limit, so set the pool of the `mrp` run to match ([below](#set-the-local-pool)).

An adaptive memory request is the one grant that can differ from `mrp`'s:
`mrp` grants the part of the pool the host has free when the job starts (the
divergence [`adaptive-memory-pool`](../reference/divergences.md)).

## `mrp --overrides`

`--mro2nf_overrides` takes the file `mrp --overrides` takes:

```bash
nextflow run main.nf --mro2nf_overrides overrides.json
```

```json
{"SUM_SQUARE_PIPELINE.SUM_SQUARES": {"chunk.mem_gb": 3, "chunk.threads": 2}}
```

PLAN reads it with Martian's own reader, so a key `mrp` does not know fails the
run before any stage starts. Each job's resources then resolve as `mrp`
resolves them, and reach both the task's `cpus` and `memory` and what the
stage sees.

- The `force_volatile` and `*.profile` keys are accepted and have no effect.
- With `-resume`, a changed overrides file re-runs the tasks of each stage
  whose resources it changes, and the tasks downstream of them.

## Size one stage

A `withName:` selector in a `-c` config sizes one stage's tasks without
changing what the stage sees. The process names are `N<i>_<CALL>`, and
`N<i>_<CALL>_SP`, `_MN` and `_JN` for a split stage's phases
([Transpiling](transpiling.md#the-generated-project)):

```groovy
process {
    withName: 'N3_ALIGN_MN' { memory = '8 GB'; cpus = 4 }
}
```

A selector also names the process inside a sub-workflow, where the trace shows
it as `W<j>:N<i>_<CALL>`.

`-resume` does not re-run a cached task because a `-c` setting changed its
`cpus` or `memory`.

## Set the local pool

The `standard` and `docker` profiles clamp each task to the pool, so a
`withName:` selector larger than the host runs instead of failing. To grant
jobs from another pool, as `mrp --localcores=8 --localmem=32` does, set it in a
`-c` config:

```groovy
process.resourceLimits = [cpus: 8, memory: 32.GB]
```

The `awsbatch`, grid and `healthomics` profiles and the HealthOmics package do
not clamp.

## Retries

A task is retried only for a failure that can pass on a second attempt, and at
the same cpus and memory, up to twice (`mrp`'s `default_retries`):

| Task exit | Meaning | Retried |
|---|---|---|
| 42 | the stage failed an `ASSERT` (`martian.exit`) | no |
| 75 | a failure `mrp` retries: a line of the job's `_errors` matches a `retry_on` pattern of `mrp`'s `jobmanagers/retry.json` (a signal, `ENOMEM`, a scheduler failure) | up to twice |
| 104, 130–145 | the executor killed the task with a signal or stopped it at a time limit, the exits nf-core pipelines retry (137 is an out-of-memory kill) | up to twice |
| 1 | a stage exception, or a failed `mre` step | no |

Under `--mro2nf_monitor true`, a job killed for exceeding its memory request is
not retried, as under `mrp` without `--auto-adjust-memory`.

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
as the task's `clusterOptions` ([Parameters](parameters.md#job_resources)).

Executors read `accelerator` differently:

- **AWS Batch** turns it into a GPU resource requirement on the job. One job
  queue can serve CPU and GPU stages: give it a CPU compute environment first
  and a GPU compute environment second. A GPU job cannot be placed on the CPU
  environment, so it goes to the GPU one. The GPU environment's instance types
  decide the GPU model and memory, and must include instances with at least as
  many GPUs as any stage requests.
- **HealthOmics** requests a GPU instance. The account needs a service quota
  for GPU instance families.
- **Local** ignores it. On a **grid**, map the `special` to the scheduler's GPU
  option with `params.job_resources = [gpu: '--gres=gpu:1']`.

A container profile runs every task in one image, so a pipeline with a GPU
stage needs one CUDA image for all its stages
([Container images](container-images.md#a-cuda-image)).
