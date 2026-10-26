# Martian feature coverage

Which Martian features a transpiled pipeline supports. "Supported" means the
e2e suite runs a fixture under both `mrp` and Nextflow and the outputs and
every stage's `_args` and `_outs` match. "Differs" means supported, with a
listed [divergence](divergences.md). Nothing unsupported fails
silently: it fails the transpile or the run's PLAN, before any stage starts.

## Language

| Feature | Status | Notes |
|---|---|---|
| stages, pipelines, a top-level call of a pipeline or a bare stage | Supported | |
| top-level `map call` | Supported | a launch value in `{"split": v}` form forks by `v` |
| entry inputs as launch parameters | Supported | PLAN recompiles the call with them, so they change forks and gates as literals would |
| entry input named with a Groovy or Nextflow keyword | Refused | Nextflow's `params` block cannot declare it (`keyword-input`) |
| `@include`, `MROPATH`, `-mropath` | Supported | |
| `filetype`, `struct`, `T[]`, `T[][]`, `map<T>`, all scalar types | Supported | |
| literals, bindings, projection through arrays and maps, `* = self` | Supported | resolved by Martian's own functions |
| struct narrowing, key order, null propagation | Supported | |
| number literal outside int64 or float64 | Refused | `mrp` panics on it |

## Calls and modifiers

| Feature | Status | Notes |
|---|---|---|
| aliased calls, nested pipelines, diamond DAGs | Supported | |
| `disabled` (static, runtime, per fork) | Supported | decided per instance as `mrp`'s `Fork.disabled` |
| `preflight` | Supported | every other call waits for it; a failure ends the run first |
| `local` | Differs | scheduled like any stage (`local-modifier-ignored`) |
| `volatile`, `retain` | Differs | no file is deleted mid-run, so `work/` keeps every intermediate (`volatile-no-vdr`, `retain-no-vdr`) |

## Map calls

| Feature | Status | Notes |
|---|---|---|
| `map call` over an array or a map | Supported | forks in `mrp`'s order |
| zipped multi-`split` | Supported | |
| map over a split stage, a sub-pipeline, another map call's outputs | Supported | fork *i* reads only upstream fork *i* |
| nested map calls | Supported | an all-empty inner split known at start fails later than under `mrp` (`nested-empty-forks`) |
| split of a runtime-gated output | Supported | |
| empty or null split collection | Supported | typed empty result, as Martian 50ad6c0 |
| a split source gathered over one map call and split by another | Differs | both fail; mro2nf fails at another stage (`post-outer-fork`) |

## Stages and resources

| Feature | Status | Notes |
|---|---|---|
| split, main, join | Supported | one task per job, chunk outputs in chunk order |
| `py`, `exec`, `comp` adapters | Supported | `py` and `comp` run under `mrjob` with `mrp`'s argv |
| `threads`, `mem_gb`, `vmem_gb`, split-returned `__*` resources | Supported | granted as the profile's `mrp` jobmode grants them |
| adaptive (negative) memory on the local profiles | Differs | the whole pool, where `mrp` grants what is free (`adaptive-memory-pool`) |
| `special`, GPUs | Supported | `params.job_resources` and `accelerator` |
| `martian` Python API | Supported | the real adapter runs |
| `martian.alarm` | Supported | written to the task's stderr; no run-level summary |
| output validation, `ASSERT` vs retryable errors | Supported | exit 42 is never retried, 75 is |
| `--overrides`, `--monitor` | Supported | `--mro2nf_overrides`, `--mro2nf_monitor` |
| `--auto-adjust-memory`, `--strict` | Not supported | a `-c` config can raise memory per retry |

## Files and outputs

| Feature | Status | Notes |
|---|---|---|
| file outputs, `outs/` layout, `GetOutFilename` naming | Supported | through Martian's `PostProcess` |
| file passing between stages, files in structs, arrays, maps | Supported | typed leaves only |
| directory outputs with dotfiles and empty directories | Supported | |
| file inputs | Differs | a staged input's path is in the task's directory (`staged-input-paths`); a file beside it is not staged (`input-file-siblings`) |
| published symlinks, `pipeline_outs.json` paths | Differs | copies, and paths relative to `results/` (`published-symlinks-copied`, `outs-json-relative-paths`) |
| a path written into text (JSON, CSV, `string`, untyped `map`) that a later stage opens | Out of scope | needs `mrp`'s shared filesystem; see [Paths written into text](usage.md#paths-written-into-text) |

## Running and inspecting

| Feature | Status | Notes |
|---|---|---|
| local and grid jobmodes | Supported | `standard`, `slurm`, `sge`, `lsf`, `pbs` profiles |
| restart | Differs | Nextflow's `-resume` caches by content (`resume-content-cache`) |
| `--maxjobs`, `--jobinterval`, `--localcores`, `--localmem` | Supported | in a `-c` config ([`mrp` equivalents](usage.md#mrp-equivalents)) |
| `--profile`, `--inspect`, web UI, `mrstat`, `mro graph` | Out of scope | Nextflow's trace, reports, `nextflow log`, `-with-dag` |
| `--onfinish` | Out of scope | a `workflow.onComplete` handler |
| VDR | Out of scope | no mid-run deletion |
