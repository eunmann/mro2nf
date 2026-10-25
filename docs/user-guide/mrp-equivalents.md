# `mrp` equivalents

Each `mrp` flag and Martian tool, and what to use with a transpiled pipeline.
Settings marked `-c` go in a config file passed with `nextflow run -c <file>`.
HealthOmics takes no `-c`.

| `mrp` | mro2nf and Nextflow |
|---|---|
| pipestance name (second argument) | `--mro2nf_psid <id>` |
| `--jobmode=MODE` | `-profile <name>` ([Profiles](profiles.md)) |
| `--localcores=N`, `--localmem=GB` | `-c`: `process.resourceLimits = [cpus: N, memory: <GB>.GB]`, the pool each job is granted from ([Resources](resources-and-retries.md#set-the-local-pool)), and `executor { cpus = N; memory = '<GB> GB' }` to run no more at once |
| `--maxjobs=N` | `-c`: `executor.queueSize = N` |
| `--jobinterval=MS` | `-c`: `executor.submitRateLimit = '<n>/1s'`, with `n` about `1000 / MS`, or `'1/<s>s'` for an interval of `s` whole seconds. Nextflow rejects an interval under one second. |
| `--overrides=JSON` | `--mro2nf_overrides <file>` |
| `--monitor` | `--mro2nf_monitor true` |
| `--autoretry=N` | `-c`: `process.maxRetries = N` |
| `--auto-adjust-memory` | not supported; [Retries](resources-and-retries.md#retries) shows a `-c` config that raises memory on each retry |
| `--profile=MODE` | no stage profiling; the trace file the project enables records each task's `realtime`, `%cpu`, `peak_rss`, `peak_vmem`, `rchar` and `wchar`, and `-with-report` adds a report |
| `--onfinish=EXEC` | `-c`: a `workflow.onComplete` handler ([below](#--onfinish)) |
| `--psdir=PATH` | the launch directory, `-work-dir` and `-output-dir` |
| `--never-local` | the default: mro2nf schedules `local` stages like any other |
| `--vdrmode=MODE` | none: mro2nf deletes no intermediate file during a run; `nextflow clean` removes work directories afterwards |
| `--nopreflight` | none: preflight stages always run; to skip one, remove its call from the `.mro` and transpile again |
| `--inspect` | none; `nextflow log` lists past runs and their tasks |
| `--uiport`, `--disable-ui`, `--disable-auth`, `--require-auth`, `--auth-key`, `--https-cert`, `--https-key`, `--noexit` | no UI; `nextflow log`, `-with-report` and `-with-timeline` |
| `--zip` | none: `mrp` zips the pipestance's metadata files; each task's work directory keeps its jobs' metadata |
| `--localvmem`, `--mempercore`, `--limit-loadavg`, `--retry-wait`, `--tags`, `--strict`, `--stackvars`, `--debug`, `--stest` | none |
| `mro graph` | `nextflow run main.nf -with-dag dag.html` |
| `mrstat` | `nextflow log` |

## `--onfinish`

`mrp` runs the `--onfinish` program with the pipestance path, `complete` or
`failed`, the pipestance ID, and the error file. A `-c` handler can pass the
output directory the run published to, the outcome and the ID:

```groovy
workflow.onComplete = {
    ['/path/to/hook', workflow.outputDir.toUriString(), workflow.success ? 'complete' : 'failed', params.mro2nf_psid].execute().waitFor()
}
```

`nextflow lint` warns about a workflow handler in a config file; the run
executes it.
