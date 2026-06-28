# Runtime tuning: mrp knobs → Nextflow

Most of mrp's launch-time knobs are runtime concerns that Nextflow already
covers — you set them on the `nextflow run` command or in a `-c` config overlay,
not in the `.mro`. This maps the mrp flags to their Nextflow equivalents.

## Per-stage resource overrides (`mrp --overrides`)

mrp's `--overrides` retunes a stage's resources at launch without editing the
`.mro`. The Nextflow analog is a `process` / `withName:` config overlay applied
with `-c`. `mro2nf` converts an existing overrides file for you:

```bash
mro2nf overrides overrides.json > overrides.config
nextflow run main.nf -c overrides.config
```

It maps each override field to the generated process names:

| mrp override field | Nextflow directive | applies to |
|---|---|---|
| `mem_gb` | `memory` | the stage (`withName: 'STAGE.*'`) |
| `threads` | `cpus` | the stage |
| `chunk.mem_gb` / `chunk.threads` | `memory` / `cpus` | the main phase (`STAGE_MAIN.*`) |
| `join.mem_gb` / `join.threads` | `memory` / `cpus` | the join phase (`STAGE_JOIN.*`) |
| `""` (the empty key) | `memory` / `cpus` | all stages (global `process {}`) |

The override key's **last segment** is taken as the stage name. Fields with no
faithful Nextflow directive are reported and skipped: `vmem_gb` (Nextflow has no
virtual-memory directive — use `mro2nf -monitor`, which reads `vmem_gb` from the
`.mro`), `force_volatile` (VDR is not modeled), and `profile` (use the trace/report
options below). mrp's *pipeline-scoped* keys (a key that names a sub-pipeline,
not a stage) don't map — our process names are stage-named, not path-qualified —
so scope overrides to individual stages.

You can also just hand-write the overlay; the process names are:
`STAGE` (non-split), `STAGE_SPLIT` / `STAGE_MAIN` / `STAGE_JOIN` (split), each
with a `_K` keyed-fork variant. A `.*` regex (e.g. `'STAGE.*'`) covers them all.

## Scheduler throttling

| mrp | Nextflow (`-c` or `nextflow.config`) |
|---|---|
| `--maxjobs N` | `executor.queueSize = N` |
| `--jobinterval MS` | `executor.submitRateLimit = '1/<MS>ms'` |
| `--localcores N` | `executor.cpus = N` (local executor) |
| `--localmem GB` | `executor.memory = 'N GB'` (local executor) |
| `--mempercore GB` | per-grid; pass via `clusterOptions` |

Example `-c`:

```groovy
executor { queueSize = 50; submitRateLimit = '10/1min' }
```

## Observability (`--profile`, `mro graph`, `--inspect`)

Nextflow has these built in — no transpiler support needed:

| mrp | Nextflow |
|---|---|
| `--profile cpu` / `mem` (stage profiling) | `nextflow run -with-trace` / `-with-report` |
| `mro graph` (DAG visualization) | `nextflow run -with-dag dag.html` |
| `--inspect` (dry-run / inspect) | `nextflow run -preview` |
| `mrstat` / web UI (live status) | `nextflow log`, `-with-report`, `-with-timeline` |

## Lifecycle hook (`--onfinish`)

mrp's `--onfinish` runs a command when the pipestance finishes. The Nextflow
analog is a `workflow.onComplete` handler in a `-c` overlay:

```groovy
workflow.onComplete = { exec("/path/to/hook ${workflow.success}") }
```

## Skipping preflight (`--nopreflight`)

There is no built-in flag. A `(preflight)` stage now gates the pipeline (it runs
first and downstream stages wait — see [`FEATURE_COVERAGE.md`](FEATURE_COVERAGE.md)),
so to skip a slow or broken validation, remove the `(preflight)` modifier (or the
call) and re-transpile.

## Out of scope by design

The web UI and its auth (`--uiport`, `--disable-ui`, `--auth-key`, `--https-*`),
`mrstat`, pipestance archiving (`--zip`), and VDR (`--vdrmode`) have no faithful
Nextflow analog — Nextflow's own logging/reporting (`nextflow log`,
`-with-report`, `-with-timeline`) is the observability layer instead. `mro2go`
(Go struct generation from `.mro`) and `mrg` (invocation generation) are
authoring-time tools orthogonal to transpilation; the run-parameter mechanism
(`-params-file`, see the README) covers supplying inputs at launch.
