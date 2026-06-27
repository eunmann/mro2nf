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

For a full end-to-end walkthrough of how the transpilation works — front end,
IR, the generated Nextflow, the bundle data plane, the `mre` shim, and the cloud
targets — see **[`docs/TRANSPILER.md`](docs/TRANSPILER.md)**.

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
test/e2e/      transpile + nextflow run + diff vs mrp (+ docker_iso.sh isolation)
deploy/awsbatch-cdk/   minimal AWS CDK: S3 + ECR + Batch + HealthOmics role
docs/           TRANSPILER.md (how it works, end to end), FEATURE_COVERAGE.md
                (support matrix), LIVE_AWS_TEST.md (validation)
.github/workflows/   PR validation + release CI
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
| `-target <backend>` | shape the project for `local` (default), `awsbatch`, or `healthomics` |
| `-monitor` | cap each stage's virtual memory at its `vmem_gb` via `prlimit` (mrp `--monitor`) |

For `-target local` the `-mre`/`-shell`/`-mrjob`/stage paths are baked into the
generated scripts, so set them to the paths that will exist **where the pipeline
runs** (the local repo, or shared-filesystem cluster paths).

For `-target awsbatch`/`healthomics` (container backends) `mart` ignores those host
paths: it bakes fixed **in-container** paths (`/opt/mart/…`), copies the `mre`
shim, the Martian adapters, and your stage code into a self-contained
`runtime/` build context, and emits a **`Dockerfile`** so `docker build` produces
the runtime image. Each task stages only the auxiliary files it needs — the
shared `types.json` plus, for a bind, its own `spec.json` — as individual `path`
inputs (never `${projectDir}`), so tasks work on an isolated worker with no
shared filesystem and transfer only their own bindspec. File-bearing entry inputs
(e.g. FASTQ) — at any shape: a file/dir, `file[]`, `map<file>`, or a struct with
file fields — are likewise staged through Nextflow, so a run parameter pointing at
an `s3://` path/prefix localizes into the task.

## Testing

```bash
make test          # unit tests (in-process, sub-second)
make test-e2e      # transpile + nextflow run + diff vs mrp, all fixtures
make lint-check    # golangci-lint (no auto-fix)
```

The e2e suite (`test/e2e/run.sh`) runs each fixture's transpiled pipeline under
Nextflow and diffs the result against the committed real-`mrp` output. It runs
cases in a bounded parallel pool — tune with `E2E_PARALLEL=<n>` (default 6) — and
includes `cloud_sim.sh` (object-store data plane under copy-staging). A separate
`make test-e2e-docker` (`test/e2e/docker_iso.sh`) runs pipelines under the
Nextflow **docker** executor — tasks in containers that mount only their work
dir — reproducing the AWS Batch / HealthOmics no-shared-filesystem model.

### How it's validated

Every transpiled pipeline is diffed against the output of the **real `mrp`**
orchestrator, so "correct" means byte-identical to Martian:

- **Unit** (`make test`): in-process tests of the IR, binder, type walk, shim
  ABI, and the generated Nextflow text.
- **Local e2e** (`make test-e2e`): 59 `.mro` fixtures transpiled, run under
  Nextflow, diffed vs committed `mrp` goldens — covering every supported feature,
  including file-typed entry inputs (scalar, `file[]`, `map<file>`,
  struct-with-file) supplied at launch via `-params-file`.
- **Container isolation** (`make test-e2e-docker`): the same pipelines under the
  docker executor (no shared filesystem), the model AWS uses — including an
  entry-file override whose input lives outside the image, so it can only arrive
  via Nextflow staging (the AWS Batch / HealthOmics S3-localization path).
- **Live AWS**: the full fixture set was run end-to-end on real **AWS Batch + S3**
  (19/19) and **AWS HealthOmics** (14/14), all byte-identical to `mrp` —
  exercising all three adapters (`py`/`exec`/`comp`), file inputs/outputs through
  the object store, directory outputs, retry/ASSERT handling, and stage logs.
  See [`docs/LIVE_AWS_TEST.md`](docs/LIVE_AWS_TEST.md) and
  [`deploy/awsbatch-cdk/`](deploy/awsbatch-cdk/) (the CDK that provisions it).

## Running on a cluster

`nextflow.config` (local target) ships executor profiles; select one with `-profile`:

```bash
nextflow run main.nf -profile slurm          # or sge / lsf / pbs / k8s
nextflow run main.nf -profile awsbatch \
    --aws_queue <job-queue> --aws_region <region> -work-dir s3://<bucket>/work
```

The **object-store data plane** makes file flow correct without a shared
filesystem: every inter-process channel item is a self-contained *bundle*
directory (its JSON payload plus the files it references), so Nextflow stages
real files across task boundaries (S3, GCS, …) instead of bare absolute paths.

## Running on AWS Batch + S3 or HealthOmics

Transpile with the matching target — `mart` bakes in-container paths and emits a
ready-to-build `Dockerfile` + `runtime/` context:

```bash
./mart -o out -target awsbatch -container <ecr-uri> \
    -mre ./mre -shell ./vendor-martian/python/martian_shell.py \
    -mropath path/to/pipeline_dir path/to/pipeline.mro

cd out
docker build --platform linux/amd64 -t <ecr-uri> .   # build the runtime image
docker push <ecr-uri>
nextflow run main.nf --aws_queue <q> --aws_region <r> \
    --container <ecr-uri> -work-dir s3://<bucket>/work
```

- **`-target awsbatch`** wires the Batch executor with classic aws-CLI S3 staging
  (the image includes the aws CLI; or set `--aws_cli_path` for a custom AMI).
- **`-target healthomics`** additionally emits `parameter-template.json` and
  `package.sh` (builds the upload `.zip`), publishes to `/mnt/workflow/pubdir`,
  sets no executor (execution is managed), and pins a supported Nextflow version.
  The image must be a **private ECR** URI in the run's region; tasks have no
  internet, so everything is baked into the image. Register with
  `aws omics create-workflow --engine NEXTFLOW --main main.nf --definition-zip
  fileb://workflow.zip --parameter-template file://parameter-template.json`.

Entry-pipeline inputs are exposed as run parameters: each becomes `params.<name>`
(defaulting to the `.mro`'s baked value), so you can override them at launch with
a Nextflow `-params-file` or AWS HealthOmics run parameters — no re-transpile:

```bash
nextflow run main.nf -params-file inputs.json     # e.g. {"values": [5, 6, 7]}
```

A minimal **AWS CDK** app in `deploy/awsbatch-cdk/` provisions exactly the infra
needed to test live (S3 bucket, ECR repo, Batch compute env + queue, and a
HealthOmics service role) — see its README for `cdk deploy` + run steps.

> Provide a **linux/amd64** `mre` (build with `GOOS=linux GOARCH=amd64`). Cloud
> file flow is validated locally via `cloud_sim.sh` (copy-staging) and
> `docker_iso.sh` (true container isolation); a live Batch/S3 or HealthOmics run
> additionally needs the AWS infrastructure (`deploy/awsbatch-cdk/`).

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
maps over pipelines with internal disabled calls, disabled nested maps, and
nested maps via 2-D fork keying). Content-based retry (ASSERT → terminate,
otherwise retry) and `-monitor` vmem enforcement are handled in the shim.

Resource provisioning tracks mrp so a transpiled run schedules — and finishes —
comparably: per-chunk `cpus`/`memory`, the split-returned `join` resource
override, and the `special` scheduler key (mapped to `clusterOptions` via a
`params.job_resources` map, the `MRO_JOBRESOURCES` analog) all reach the
executor, and every phase reports its resolved allocation. Documented divergences
(output-correct; only layout/timing differ): mid-run VDR deletion, nested maps on
a true object-store work-dir, and mrp's nested `outs/` tree vs Nextflow's flat
`publishDir`.

## Development

`make help` lists all targets. House rules live in `CLAUDE.md` and
`.claude/skills/`. In short: no behavior changes without tests, functions ≤ 70
lines, wrap errors with `%w`, standard `testing` + `go-cmp` only, and
`make lint-check` must pass.

CI (`.github/workflows/pr-validation.yml`) runs lint (golangci-lint + `govulncheck`
+ a `go mod tidy` check), build, unit tests (`-race`), and the e2e suites on every
PR and push to `main`. The Martian dependency is pinned to a published commit, so
CI needs only a single checkout; to hack on the Martian parser locally, add a
`go.work` pointing at a fork checkout (gitignored). Tagging `v*` triggers
`release.yml`, which cross-compiles the `mart` and `mre` binaries and publishes a
GitHub release.

## License

MIT — see [`LICENSE`](LICENSE).
