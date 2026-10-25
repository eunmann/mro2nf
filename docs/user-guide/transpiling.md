# Transpiling a pipeline

```bash
mro2nf [-o <dir>] [-mropath <path>] <pipeline.mro>
```

| Argument | Meaning |
|---|---|
| `<pipeline.mro>` | the entry file: the pipeline and the `call` `mrp` would run. The `call`'s arguments become the run's default inputs. |
| `-o <dir>` | the project directory (default `out`) |
| `-mropath <path>` | the `@include` search path, `:`-separated. The default is `$MROPATH` when it is set, else the `.mro`'s directory, as under `mrp`. The working directory is never searched. |
| `-version` | print the version and exit |

The entry's own directory is searched first for its own includes and its
stages' code.

## What fails the transpile

mro2nf compiles the pipeline with Martian's own compiler, so it fails, and
writes no project, wherever `mrp` would refuse to start, with `mrp`'s message.
It also fails on:

- the few shapes whose data the tasks cannot exchange, listed in
  [Feature coverage](../reference/feature-coverage.md);
- an entry input named `job_resources` or `mro2nf_*`, because the project
  defines those parameters itself;
- an entry input named with a Groovy or Nextflow keyword, such as `def`,
  `new` or `class`, which Nextflow's `params` block refuses, quoted or not
  (the divergence [`keyword-input`](../reference/divergences.md));
- a number literal out of range: an int outside int64, a float beyond
  float64, or a `threads`, `mem_gb` or `vmem_gb` beyond float32, on which
  `mrp` panics.

## Keep the code where it was

The project refers to the entry `.mro`, its includes and its stages' code by
their absolute paths at transpile time. Every profile runs the code from those
paths, so don't move them. A container image holds the code at the same paths
([Container images](container-images.md)).

That includes the entry `.mro` itself: an invocation written outside the code
tree, such as the `__<id>.mro` that `cellranger count --dry` writes, goes into
the image at its own path too.

## The generated project

| File | Purpose |
|---|---|
| `main.nf` | the entry workflow: PLAN, the stage calls or the workflows that run them, then OUTS |
| `subworkflows/` | only in a pipeline too large for one workflow: one workflow per run of its stage calls |
| `stage.nf` | the stage processes and the wiring every stage call uses |
| `nextflow.config` | the oldest Nextflow the project runs on, `conf/base.config`, the profiles |
| `nextflow_schema.json` | every parameter's type, default and description, in the nf-schema format |
| `conf/` | `base.config`, the settings every profile shares (parameters, retry policy, GPU rule, trace), and the container and HealthOmics profiles' settings |
| `package.sh`, `parameter-template.json` | the HealthOmics package ([Profiles](profiles.md#aws-healthomics)) |
| `_assets/` | a copy of the `.mro` files, read by PLAN and OUTS |

Each stage call is a process named `N<i>_<CALL>`. A split stage has three
processes, `N<i>_<CALL>_SP`, `_MN` and `_JN`, for its split, chunk (main) and
join phases. The `include` lines at the top of `main.nf` list them all. Use
these names in `withName:` selectors
([Resources and retries](resources-and-retries.md#size-one-stage)).

A run writes `work/`, `results/`, `.nextflow/`, `.nextflow.log` and
`trace-<timestamp>.txt` into the launch directory
([Getting started](../getting-started.md#outputs)).

[Transpile time](../architecture/transpile.md) explains how the project is
built.
