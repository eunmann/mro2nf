# martian-nextflow

Transpile [Martian](https://martian-lang.org) (`.mro`) pipelines into runnable
[Nextflow](https://www.nextflow.io) projects — **without rewriting your stage
code**.

The transpiler translates only the *orchestration* (the DAG, splits, map-call
forks, bindings, resources) into idiomatic Nextflow DSL2. Each generated process
runs your **original, unmodified Martian stage code** through the real Martian
adapter ABI, driven by a small shim (`mre`). You escape the `mrp` orchestrator
and gain Nextflow's executors (local, SLURM/SGE/LSF/PBS, AWS Batch, Kubernetes)
and its object-store data plane — while the stage code never knows it moved.

```
.mro + stage code ──mart──▶ Nextflow project ──nextflow run──▶ results/
                            (main.nf, modules/,                (== mrp outputs)
                             nextflow.config, types.json)
                                    │ each process:
                                    ▼
                            mre <phase> → martian_shell.py → your stage code
```

## Why it works

- Martian is **filesystem-mediated**: a stage phase is `<adapter> <split|main|join>
  <metadata> <files> <journal>`, reading `_args`/`_jobinfo` and writing
  `_outs`/`_stage_defs`. The shim reproduces exactly what `mrp`+`mrjob` do for one
  phase.
- Martian's **topology is static** (only chunk *counts* are dynamic), which maps
  cleanly onto Nextflow's static-DAG / runtime-cardinality model.
- Every stage is uniformly `split → chunks → join` (a non-split stage is the
  1-chunk degenerate case).
- The Martian parser/resolver is a public Go package, so we import it rather than
  reimplement the grammar.

## Layout

```
cmd/mart/      transpiler CLI: .mro -> Nextflow project
cmd/mre/       runtime shim: runs one stage phase against the Martian adapter
internal/
  frontend/    parse .mro via github.com/martian-lang/martian/syntax -> IR
  ir/          normalized transpiler IR
  emit/        IR -> Nextflow (main.nf, modules, nextflow.config)
  types/       type-directed file-leaf walk + scalar coercion + manifest
  shim/        _args/_jobinfo/_outs I/O, bundle data plane, adapter launch
  bind/        resolve call bindings into _args (refs, projections, fan-in)
  logging/     zerolog setup    apperror/  typed errors
vendor-martian/python/   pinned martian_shell.py + martian.py adapters
testdata/      .mro fixtures with expected mrp outputs (e2e goldens)
test/e2e/      transpile + nextflow run + diff vs mrp
docs/FEATURE_COVERAGE.md   every Martian feature -> support status -> test
```

## Quickstart

Prerequisites: Go ≥ 1.24, Java 17+, [Nextflow](https://www.nextflow.io)
(`curl -s https://get.nextflow.io | bash`), Python 3.

```bash
make build                      # builds ./mart and ./mre

./mart -o out \
    -mre "$PWD/mre" \
    -shell "$PWD/vendor-martian/python/martian_shell.py" \
    -mropath path/to/pipeline_dir \
    path/to/pipeline.mro

cd out && nextflow run main.nf  # results land in out/results/
```

### `mart` flags

| Flag | Meaning |
|---|---|
| `-o <dir>` | output directory for the Nextflow project (default `out`) |
| `-mropath <path>` | `@include` search path (`:`-separated) |
| `-mre <path>` | path to the `mre` shim binary as it will appear **at run time** |
| `-shell <path>` | path to `martian_shell.py` at run time |
| `-mrjob <path>` | path to `mrjob` (only needed for `comp` stages) |
| `-container <image>` | set `process.container` for container backends (AWS Batch, k8s) |
| `-monitor` | cap each stage's virtual memory at its `vmem_gb` via `prlimit` (mrp `--monitor`) |

`-mre`/`-shell`/`-mrjob`/stage paths are baked into the generated scripts, so set
them to the paths that will exist **where the pipeline runs** (the local repo for
local runs; the in-container paths for a containerized backend — see below).

## Testing

```bash
make test          # unit tests (in-process, sub-second)
make test-e2e      # transpile + nextflow run + diff vs mrp, all fixtures
make lint-check    # golangci-lint (no auto-fix)
```

The e2e suite (`test/e2e/run.sh`) runs each fixture's transpiled pipeline under
Nextflow and diffs the result against the committed real-`mrp` output. It runs
cases in a bounded parallel pool — tune with `E2E_PARALLEL=<n>` (default 6) — and
includes `cloud_sim.sh`, which exercises the object-store data plane under
copy-staging into isolated scratch dirs.

## Running on a cluster or the cloud

`nextflow.config` ships executor profiles; select one with `-profile`:

```bash
nextflow run main.nf -profile slurm          # or sge / lsf / pbs / k8s
nextflow run main.nf -profile awsbatch \
    --aws_queue <job-queue> --aws_region <region> -work-dir s3://<bucket>/work
```

The **object-store data plane** makes file flow correct without a shared
filesystem: every inter-process channel item is a self-contained *bundle*
directory (its JSON payload plus the files it references), so Nextflow stages
real files across task boundaries (S3, GCS, …) instead of bare absolute paths.

For a **container backend** (AWS Batch, Kubernetes), build an image containing
the `mre` shim, `vendor-martian/python/` adapters (plus `mrjob` for `comp`
stages), your stage code, and `aws-cli` (Batch needs it for S3 staging) at fixed
paths, then transpile against those paths:

```bash
./mart -o out -container <ecr-uri> \
    -mre /opt/mre -shell /opt/martian/martian_shell.py -mropath /opt/pipeline \
    /opt/pipeline/pipeline.mro
```

> Cloud support is validated locally via `cloud_sim.sh` (copy-staging); a real
> Batch/S3 run additionally needs the container image and AWS infrastructure,
> which are outside this repo.

## Feature coverage & limitations

`docs/FEATURE_COVERAGE.md` is the authoritative matrix — every Martian feature
(from a page-by-page audit of all seven martian-lang.org docs, cross-checked
against the `martian/syntax` grammar) mapped to its support status and the test
that exercises it.

Supported: stages/pipelines/calls, split/main/join, map calls over arrays/maps/
**split stages**/**sub-pipelines**, structs & typed maps (including
`map<S>.field` projection), file/array/map/struct-of-file outputs, the `py`/
`exec`/`comp` adapters, call modifiers, dynamic per-chunk resources, and the
object-store data plane.

Every Martian map-call combination is supported (including disabled map calls,
maps over pipelines with internal disabled calls, and nested maps via 2-D fork
keying). Content-based retry (ASSERT → terminate, otherwise retry) and `-monitor`
vmem enforcement are handled in the shim. Documented divergences (output-correct;
only allocation/layout/timing differ): mid-run VDR deletion, the `special`
scheduler key (map it via a `withName`/`clusterOptions` config block), and mrp's
nested `outs/` tree vs Nextflow's flat `publishDir`.

## Development

`make help` lists all targets. House rules live in `CLAUDE.md` and
`.claude/skills/`. In short: no behavior changes without tests, functions ≤ 70
lines, wrap errors with `%w`, standard `testing` + `go-cmp` only, and
`make lint-check` must pass.
