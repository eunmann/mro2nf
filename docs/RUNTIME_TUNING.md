# Runtime tuning: mrp knobs → Nextflow

Most of mrp's launch-time knobs are runtime concerns that Nextflow already
covers — you set them on the `nextflow run` command or in a `-c` config overlay,
not in the `.mro`. This maps the mrp flags to their Nextflow equivalents.

## Per-stage resource overrides (`mrp --overrides`)

mrp's `--overrides` file is not accepted at launch yet: #301 applies it in the
run's PLAN task with Martian's own `ApplyResourceOverrides`, so each stage
instance is sized, and sees in `_jobinfo`, what mrp gives it.

Until then, a `withName:` selector in a `-c` overlay sizes one stage's tasks.
Each stage node is its own process alias, `N<i>_<CALL>` (the node's index in
`main.nf` and its call name), and a split stage's phases are `N<i>_<CALL>_SP`,
`_MN` and `_JN`; `main.nf` lists every alias in its `include` lines:

```groovy
process {
    withName: 'N3_ALIGN_MN' { memory = '8 GB'; cpus = 4 }
}
```

Such an overlay only sizes the task: the stage still sees its `.mro` request in
`__mem_gb`/`__threads` and `_jobinfo`, and `-monitor` still enforces that
request.

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

mrp clamps a job's request to its `--localcores`/`--localmem` pool and runs it.
The generated `standard` profile does the same against the whole host, through
`process.resourceLimits`; Nextflow's local executor fails a task whose request
exceeds `executor.cpus`/`executor.memory`. To shrink the pool, set the clamp
with it:

```groovy
executor { cpus = 8; memory = '16 GB' }
process.resourceLimits = [ cpus: 8, memory: 16.GB ]
```

## Observability (`--profile`, `mro graph`, `--inspect`)

Nextflow has these built in — no transpiler support needed:

| mrp | Nextflow |
|---|---|
| `--profile cpu` / `mem` (stage profiling) | the task trace the generated config enables (`trace-<timestamp>.txt` in the launch dir: status, exit, attempt, realtime, peak RSS, work dir; local and AWS Batch targets), `-with-report` |
| `mro graph` (DAG visualization) | `nextflow run -with-dag dag.html` |
| `--inspect` (dry-run / inspect) | `nextflow run -preview` |
| `mrstat` / web UI (live status) | `nextflow log`, `-with-report`, `-with-timeline` |

## Lifecycle hook (`--onfinish`)

mrp's `--onfinish` runs a command when the pipestance finishes. The Nextflow
analog is a `workflow.onComplete` handler in a `-c` overlay:

```groovy
workflow.onComplete = { ["/path/to/hook", "${workflow.success}"].execute().waitFor() }
```

## Skipping preflight (`--nopreflight`)

There is no built-in flag. A plain `(preflight)` stage now gates the pipeline (it
runs first and downstream stages wait — see [`FEATURE_COVERAGE.md`](FEATURE_COVERAGE.md)),
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
