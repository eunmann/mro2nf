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
cmd/mro2nf/      transpiler CLI: .mro -> Nextflow project (+ `overrides` subcommand)
cmd/mre/       runtime shim: runs stage phases (split|main|join) against the
               Martian adapter, plus the data-plane subcommands the generated
               processes call (bind, forkbind, merge, publish-layout, entryargs)
internal/
  frontend/    parse .mro via github.com/martian-lang/martian/syntax -> IR
  ir/          normalized transpiler IR
  emit/        IR -> Nextflow (main.nf, modules, nextflow.config)
  types/       type-directed file-leaf walk + scalar coercion + manifest
  shim/        _args/_jobinfo/_outs I/O, bundle data plane, adapter launch
  bind/        resolve call bindings into _args (refs, projections, fan-in)
  overrides/   mrp --overrides file -> Nextflow -c config
  config/      .mro2nf.yml project defaults for the transpiler flags
  logging/     zerolog setup    apperror/  typed errors
vendor-martian/python/   pinned martian_shell.py + martian.py adapters
testdata/      .mro fixtures with expected mrp outputs (e2e goldens)
test/e2e/      Go e2e suites (golden diff vs mrp, docker isolation) + AWS runbooks
deploy/awsbatch-cdk/   minimal AWS CDK: S3 + ECR + Batch + HealthOmics role
docs/           TRANSPILER.md (how it works, end to end), FEATURE_COVERAGE.md
                (support matrix), GPU.md, RUNTIME_TUNING.md (mrp knobs ->
                Nextflow), LIVE_AWS_TEST.md (validation)
.github/workflows/   PR validation + release CI
```

## Quickstart

Prerequisites: Go ≥ 1.26, Java 17+, [Nextflow](https://www.nextflow.io)
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
| `-monitor` | enforce mrp `--monitor`'s memory limits per stage: an RSS kill at `mem_gb` (the process-group monitor, mrp's primary kill) plus a `prlimit` virtual-memory cap at `vmem_gb` |
| `-fuse-chains` | fuse a single-consumer, equal-resource source stage into its consumer's task (fewer tasks; **trade-off:** `-resume` and per-stage retry are coarser for the fused stages) |
| `-fold-disables` | constant-fold an entry-determinable disable gate: an always-disabled stage is pruned at transpile time (**trade-off:** overriding that gate input at launch will *not* re-enable the pruned stage — the transpiler warns which) |
| `-native` | channel-native orchestration: collapse the data-plane tasks into driver channel wiring (see [Native mode](#native-mode--native--native-runner)). **Trade-off:** entry inputs are baked at transpile time — supplying one at launch is a loud error; re-transpile to change them |
| `-native-runner` | run `py` stages through the embedded direct-call runner instead of `mre` + `martian_shell.py` (`exec`/`comp` stages keep the adapter). On container backends the runner is baked into the image, so toggling it there means an image rebuild |
| `-config <path>` | path to a [`.mro2nf.yml`](#mro2nfyml-project-defaults) of per-project flag defaults (default: probed alongside the `.mro`) |
| `-version` | print the mro2nf version and exit |

`mro2nf overrides <file>` converts an mrp `--overrides` JSON into an equivalent
Nextflow `-c` config overlay (per-stage `memory`/`cpus` retuning at launch) —
see [`docs/RUNTIME_TUNING.md`](docs/RUNTIME_TUNING.md).

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

### Native mode (`-native`, `-native-runner`)

By default every piece of orchestration between stage tasks — binding, forking,
merging, entry-arg resolution — runs as its own small task. `-native` collapses
that data plane into driver-side channel wiring while leaving the stage tasks
(and the Martian adapter ABI they speak) untouched:

- **Entry args are baked at transpile time** (`entry_resolved/` in the project;
  no `BUILD_ENTRY_ARGS` task). Supplying an entry parameter at launch is a
  **loud error** — re-transpile to change an input.
- **Plain calls fuse** their bind into the stage's own task; simple disable
  gates are branched on the driver instead of a `DISABLE` task.
- **Eligible map calls scatter in-workflow with no FORK task**: the driver
  slices the fork collection once and hands each fused instance only its own
  element (O(1) per fork, not O(N) re-parses).
- **Sole-consumer MERGE gathers fold** into the consumer's task.

Shapes that can't fully collapse keep a bounded remainder — e.g. a
file-bearing, multi-split, or projected fork keeps **one** FORK resolve task
(O(total data), never per-instance), a disabled map call keeps its MERGE as the
skip-branch mix point, and a map over a split stage or sub-pipeline runs its
fork-keyed layer. **Every non-collapsed shape prints an Info diagnostic at
transpile time** naming exactly which tasks remain and why, so a partial
collapse is never silent (see `internal/emit/diagnostics.go`).

`-native-runner` is independent: it swaps the *stage-execution* hop for `py`
stages from `mre` + `martian_shell.py` to an embedded direct-call Python runner
(`import martian` resolves to a shipped compat shim). `exec`/`comp` stages keep
the adapter path (an Info diagnostic names each). It composes with or without
`-native`, and on container backends the runner is baked into the image.

Both modes are held to the same bar as the default emission: the native e2e
suites diff their outputs against the committed real-`mrp` goldens, and pin the
exact set of surviving data-plane processes per pipeline shape (see
[`docs/FEATURE_COVERAGE.md`](docs/FEATURE_COVERAGE.md)).

### `.mro2nf.yml` project defaults

A `.mro2nf.yml` next to the pipeline `.mro` (or named via `-config`, in which
case it must exist) sets per-project defaults for the transpile flags, so a
team doesn't have to repeat them on every invocation:

```yaml
# keys mirror the CLI flags
target: awsbatch
container: 123456789.dkr.ecr.us-east-1.amazonaws.com/pipe:1
native: true
native-runner: true
```

Supported keys: `target`, `container`, `mre`, `shell`, `mrjob`, `monitor`,
`fuse-chains`, `fold-disables`, `native`, `native-runner`. Precedence is
**builtin default < config file < explicit flag**. To override a config `true`
back off for one run, use the equals form — `-native=false` — because Go's flag
parsing reads a space-separated `-native false` as a bare `-native` plus a
positional argument. Unknown keys and malformed values are errors, so typos are
loud.

## Testing

```bash
make test          # unit tests (in-process, sub-second)
make cover         # unit-test coverage gate (fails below COVER_MIN%)
make test-e2e      # golden table + cloud-sim + failure paths + launch knobs
make test-e2e-docker  # the same pipelines under docker (container isolation)
make lint-check    # golangci-lint (no auto-fix)
```

The e2e suites are Go tests in `test/e2e` (build tag `e2e`, always run with
`-count=1` so the test cache can't return a stale ok). `make test-e2e` runs
each fixture's transpiled pipeline under Nextflow in a bounded parallel pool
(`E2E_PARALLEL=<n>`, default 10) and diffs the result against the committed
real-`mrp` golden; it also covers copy-staging (the object-store data plane),
the retry/ASSERT failure contract, `-resume` cache stability, and
`mro2nf overrides` overlay application. `make test-e2e-docker` runs pipelines
under the Nextflow **docker** executor — tasks in containers that mount only
their work dir — reproducing the AWS Batch / HealthOmics no-shared-filesystem
model, and additionally builds + runs the *generated* `-target awsbatch`
Dockerfile and validates the HealthOmics package. The live AWS runbooks
(`test/e2e/aws_*.sh`) and the informational mrp oracle (`mapcall_matrix.sh`)
remain shell.

### How it's validated

Every transpiled pipeline is diffed against the output of the **real `mrp`**
orchestrator, so "correct" means byte-identical to Martian:

- **Unit** (`make test`): in-process tests of the IR, binder, type walk, shim
  ABI, and the generated Nextflow text.
- **Local e2e** (`make test-e2e`): the golden fixture table — `.mro` fixtures
  transpiled, run under Nextflow, diffed vs committed `mrp` goldens — covering
  every supported feature, including file-typed entry inputs (scalar, `file[]`,
  `map<file>`, struct-with-file) supplied at launch via `-params-file`.
- **Container isolation** (`make test-e2e-docker`): a representative fixture
  slice — including the exec/comp adapters and the null-bundle / zero-chunk
  shapes — and the file-typed entry-input overrides under the docker executor
  (no shared filesystem), the model AWS uses, plus a build-and-run of the
  *generated* `-target awsbatch` Dockerfile and HealthOmics package validation.
  Includes an entry-file override whose input lives outside the image, so it
  can only arrive via Nextflow staging (the AWS Batch / HealthOmics
  S3-localization path), and a same-basename `file[]` override that proves the
  per-leaf staging never collides.
- **Live AWS**: the fixture set has been run end-to-end on real **AWS Batch + S3**
  and **AWS HealthOmics**, all byte-identical to `mrp` — exercising all three
  adapters (`py`/`exec`/`comp`), file inputs/outputs through the object store,
  directory outputs, retry/ASSERT handling, and stage logs. The most recent round
  (round 5, after the data-plane epic) re-validated 13 fixtures in parallel on
  Batch and 4 on HealthOmics, with the emitted `manifest.json.gz` verified
  set-equal to the published S3 outputs on both backends.
  See [`docs/LIVE_AWS_TEST.md`](docs/LIVE_AWS_TEST.md) and
  [`deploy/awsbatch-cdk/`](deploy/awsbatch-cdk/) (the CDK that provisions it).

## Running on a cluster

`nextflow.config` (local target) ships executor profiles; select one with `-profile`:

```bash
nextflow run main.nf -profile slurm          # or sge / lsf / pbs / k8s
nextflow run main.nf -profile awsbatch \
    --aws_queue <job-queue> --aws_region <region> -work-dir s3://<bucket>/work
```

The object-store data plane is what makes file flow work without a shared
filesystem. Every payload between processes travels as a typed JSON sidecar
plus the files it references as individual `path` items; Nextflow stages those
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
instead — it ships `mre`, the Martian adapters, and the `-native-runner` runner
at `/opt/mro2nf`, multi-arch:

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
> file flow is validated locally via the copy-staging suite (`TestCloudSim*`) and
> the docker-isolation suite (`make test-e2e-docker`); a live Batch/S3 or HealthOmics run
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
terminates, everything else retries) and `-monitor` memory enforcement (RSS
kill + vmem cap).

Resources track mrp, so a transpiled run schedules and finishes comparably.
Per-chunk `cpus` and `memory`, the split-returned `join` override, and the
`special` scheduler key all reach the executor, and every phase reports the
allocation it resolved. (The `special` key maps to `clusterOptions` through a
`params.job_resources` map, the `MRO_JOBRESOURCES` analog.) The reserved
`special = "gpu"` / `"gpu:N"` instead requests N GPUs via the `accelerator`
directive on the compute phase — see [`docs/GPU.md`](docs/GPU.md) for the AWS
Batch / HealthOmics setup. A few things diverge
in timing or mechanism but never in output: there is no mid-run VDR deletion
(work dirs are retained), nested maps stage whole bundle dirs on a real
object-store work dir, and the published `outs/` tree is copied into place
(mrp moves + symlinks) — the tree's layout and contents match mrp's.

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
