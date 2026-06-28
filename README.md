# mro2nf

Turn [Martian](https://martian-lang.org) (`.mro`) pipelines into runnable
[Nextflow](https://www.nextflow.io) projects, without rewriting your stage code.

The transpiler translates only the orchestration: the DAG, splits, map-call
forks, bindings, and resource requests. It does not touch your stage code. Each
generated Nextflow process runs your original Martian stage code through the real
Martian adapter, driven by a small shim called `mre`. You leave the `mrp`
orchestrator behind and gain Nextflow's executors (local, SLURM, SGE, LSF, PBS,
AWS Batch, Kubernetes) and its object-store data plane. The stage code runs
unchanged and never knows it moved.

```
.mro + stage code ──mro2nf──▶ Nextflow project ──nextflow run──▶ results/
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

- Martian is filesystem-mediated. A stage phase runs as `<adapter> <split|main|join>
  <metadata> <files> <journal>`: it reads `_args` and `_jobinfo` and writes
  `_outs` and `_stage_defs`. The shim reproduces exactly what `mrp` and `mrjob` do
  for one phase.
- Martian's topology is static (only chunk counts are decided at runtime), so it
  maps cleanly onto Nextflow's model of a static DAG with runtime cardinality.
- Every stage has the same shape: split into chunks, run each chunk, join. A
  non-split stage is just the one-chunk case.
- The Martian parser and resolver are a public Go package, so we import them
  instead of reimplementing the grammar.

## Layout

```
cmd/mro2nf/      transpiler CLI: .mro -> Nextflow project
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
                (support matrix), GPU.md, RUNTIME_TUNING.md (mrp knobs ->
                Nextflow), LIVE_AWS_TEST.md (validation)
.github/workflows/   PR validation + release CI
```

## Quickstart

Prerequisites: Go ≥ 1.24, Java 17+, [Nextflow](https://www.nextflow.io)
(`curl -s https://get.nextflow.io | bash`), Python 3.

```bash
make build                      # builds ./mro2nf and ./mre

./mro2nf -o out \
    -mre "$PWD/mre" \
    -shell "$PWD/vendor-martian/python/martian_shell.py" \
    -mropath path/to/pipeline_dir \
    path/to/pipeline.mro

cd out && nextflow run main.nf  # results land in out/results/
```

### `mro2nf` flags

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

For `-target awsbatch` and `-target healthomics` (container backends) `mro2nf`
ignores those host paths. It bakes fixed in-container paths under `/opt/mro2nf/`,
copies the `mre` shim, the Martian adapters, and your stage code into a
self-contained `runtime/` build context, and writes a `Dockerfile` so
`docker build` produces the runtime image.

These backends have no shared filesystem, so each task stages in only the files
it needs as individual `path` inputs (never via `${projectDir}`): the shared
`types.json`, plus its own `spec.json` for a bind. File-bearing entry inputs (a
FASTQ, say) are staged the same way, at any shape: a file or directory, a
`file[]`, a `map<file>`, or a struct with file fields. So a run parameter that
points at an `s3://` path or prefix is localized into the task by Nextflow.

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
- **Container isolation** (`make test-e2e-docker`): 19 checks (14 fixtures + 5
  entry-file overrides) under the docker executor (no shared filesystem), the
  model AWS uses — including an entry-file override whose input lives outside the
  image, so it can only arrive via Nextflow staging (the AWS Batch / HealthOmics
  S3-localization path), and a same-basename `file[]` override that proves the
  per-leaf staging never collides.
- **Live AWS**: the fixture set has been run end-to-end on real **AWS Batch + S3**
  and **AWS HealthOmics**, all byte-identical to `mrp` — exercising all three
  adapters (`py`/`exec`/`comp`), file inputs/outputs through the object store,
  directory outputs, retry/ASSERT handling, and stage logs. The most recent round
  re-ran 15 fixtures in parallel on Batch after the entry-file staging fix,
  including a same-basename `file[]` case that proves per-leaf staging never
  collides. See [`docs/LIVE_AWS_TEST.md`](docs/LIVE_AWS_TEST.md) and
  [`deploy/awsbatch-cdk/`](deploy/awsbatch-cdk/) (the CDK that provisions it).

## Running on a cluster

`nextflow.config` (local target) ships executor profiles; select one with `-profile`:

```bash
nextflow run main.nf -profile slurm          # or sge / lsf / pbs / k8s
nextflow run main.nf -profile awsbatch \
    --aws_queue <job-queue> --aws_region <region> -work-dir s3://<bucket>/work
```

The object-store data plane is what makes file flow work without a shared
filesystem. Every channel item between processes is a self-contained *bundle*
directory: its JSON payload plus the files it references. Nextflow stages those
real files across task boundaries (S3, GCS, and so on) instead of passing bare
absolute paths.

## Running on AWS Batch + S3 or HealthOmics

Transpile with the matching target — `mro2nf` bakes in-container paths and emits a
ready-to-build `Dockerfile` + `runtime/` context:

```bash
./mro2nf -o out -target awsbatch -container <ecr-uri> \
    -mre ./mre -shell ./vendor-martian/python/martian_shell.py \
    -mropath path/to/pipeline_dir path/to/pipeline.mro

cd out
docker build --platform linux/amd64 -t <ecr-uri> .   # build the runtime image
docker push <ecr-uri>
nextflow run main.nf --aws_queue <q> --aws_region <r> \
    --container <ecr-uri> -work-dir s3://<bucket>/work
```

The emitted `Dockerfile` is self-contained (it vendors `mre` + the adapters). If
you'd rather not vendor them, base your image on the published runtime image
instead — it ships `mre` + the Martian adapters at `/opt/mro2nf`, multi-arch:

```dockerfile
FROM ghcr.io/eunmann/mro2nf-runtime:<version>   # match your mro2nf version
COPY stages /opt/mro2nf/stages                  # your stage code
# comp-adapter stages also need an mrjob at /opt/mro2nf/mrjob
```

Use the tag matching the `mro2nf` version that produced the project, so the `mre`
ABI matches the generated scripts. The image is published on each release (see
`deploy/runtime/Dockerfile` and the `release.yml` `image` job).

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

`docs/FEATURE_COVERAGE.md` is the authoritative matrix. It comes from a
page-by-page read of all seven martian-lang.org docs, cross-checked against the
`martian/syntax` grammar, and maps every Martian feature to its support status
and the test that exercises it.

Supported: stages, pipelines, and calls; split/main/join; map calls over arrays,
maps, split stages, and sub-pipelines; structs and typed maps (including
`map<S>.field` projection); file, array, map, and struct-of-file outputs; the
`py`, `exec`, and `comp` adapters; call modifiers; dynamic per-chunk resources;
and the object-store data plane.

Every map-call combination works, including disabled map calls, maps over
pipelines that contain disabled calls, disabled nested maps, and nested maps
(handled by 2-D fork keying). The shim handles content-based retry (ASSERT
terminates, everything else retries) and `-monitor` vmem enforcement.

Resources track mrp, so a transpiled run schedules and finishes comparably.
Per-chunk `cpus` and `memory`, the split-returned `join` override, and the
`special` scheduler key all reach the executor, and every phase reports the
allocation it resolved. (The `special` key maps to `clusterOptions` through a
`params.job_resources` map, the `MRO_JOBRESOURCES` analog.) The reserved
`special = "gpu"` / `"gpu:N"` instead requests N GPUs via the `accelerator`
directive on the compute phase — see [`docs/GPU.md`](docs/GPU.md) for the AWS
Batch / HealthOmics setup. A few things diverge
in layout or timing but never in output: mid-run VDR deletion, nested maps on a
real object-store work dir, and mrp's nested `outs/` tree versus Nextflow's flat
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
`release.yml`, which cross-compiles the `mro2nf` and `mre` binaries and publishes a
GitHub release.

## License

MIT — see [`LICENSE`](LICENSE).
