# mro2nf

Turn [Martian](https://martian-lang.org) (`.mro`) pipelines into runnable
[Nextflow](https://www.nextflow.io) projects, without rewriting your stage code.

The transpiler translates only the orchestration: the DAG, splits, map-call
forks, bindings, and resource requests. It does not touch your stage code. The
generated project runs one Nextflow process per stage node of Martian's own
compiled call graph, and each task runs your original Martian stage code through
`mrjob` and the real Martian adapter, started by a small shim called `mre` as
`mrp` starts it. You leave the `mrp` orchestrator behind and gain Nextflow's
executors (local, SLURM, SGE, LSF, PBS) and its `-resume` cache. The stage code
runs unchanged and never knows it moved.

```
.mro + stage code ──mro2nf──▶ Nextflow project ──nextflow run──▶ results/
                            (main.nf, stage.nf,                (== mrp outputs)
                             nextflow.config, _assets/)
                                    │ each stage task:
                                    ▼
                    mre args → mre run → mrjob → martian_shell.py → your stage code
```

For a full end-to-end walkthrough of how the transpilation works — the witness
call graph, the generated Nextflow, the `mre` shim, and the bundles tasks
exchange — see **[`docs/TRANSPILER.md`](docs/TRANSPILER.md)**.

## Why it works

- Martian is filesystem-mediated. A stage phase runs as `<adapter> <split|main|join>
  <metadata> <files> <journal>`: it reads `_args` and `_jobinfo` and writes
  `_outs` and `_stage_defs`. `mre run` writes exactly what `mrp` writes for one
  job and runs it through the same `mrjob`.
- Martian's topology is static (only chunk counts and data-dependent forks are
  decided at runtime), so it maps cleanly onto Nextflow's model of a static DAG
  with runtime cardinality.
- The Martian compiler, call graph and resolver are a public Go package, so mre
  imports them instead of reimplementing the grammar or the binding rules.

## Layout

```
cmd/mro2nf/    transpiler CLI: .mro -> Nextflow project
cmd/mre/       runtime shim the generated tasks call:
               plan | args | run | post | outs
internal/
  cg/          Martian's compiled call graph: witness, topology, plan, resolver
  cgnf/        call graph -> Nextflow project (main.nf wiring, stage.nf module)
  job/         one stage job through mrjob, as mrp runs it
  frontend/    lower a Martian AST to the typed parameters of the type manifest
  ir/          typed parameters of callables and structs
  types/       type-directed file-leaf walk + type manifest
  shim/        bundle I/O, markers, output confinement
  logging/     slog setup
vendor-martian/python/   pinned martian_shell.py + martian.py adapters, which mre embeds (Martian's LICENSE beside)
THIRD_PARTY_NOTICES      licenses of the code the binaries link and the images vendor
testdata/      .mro fixtures with expected mrp outputs (e2e goldens)
test/e2e/      Go e2e suites (golden diff vs mrp, mrp differential)
test/gen/      generator of random well-typed programs for the generative differential
test/thirdparty/   THIRD_PARTY_NOTICES generator, vendored-license and vendor-martian drift checks
scripts/aws/   normcmp.py, the path-normalized outs comparison of the live AWS runs
deploy/        the release runtime image; a minimal AWS CDK (S3 + ECR + Batch + HealthOmics role)
docs/          TRANSPILER.md (how it works, end to end), FEATURE_COVERAGE.md
               (support matrix), GPU.md, RUNTIME_TUNING.md (mrp knobs ->
               Nextflow), BENCHMARKS.md
.github/workflows/   PR validation, nightly and release CI
```

## Quickstart

Prerequisites: Go ≥ 1.26, Java 17+ (CI and recommended: Temurin 25 LTS;
Nextflow 26.04 supports up to Java 26, the 24.04 floor up to 21),
[Nextflow](https://www.nextflow.io) ≥ 24.04 (`curl -s https://get.nextflow.io | bash`),
Python 3.

```bash
make install                    # mro2nf, mre and mrjob (the pinned Martian's) into ~/.local/bin

mro2nf -o out path/to/pipeline.mro

cd out && nextflow run main.nf  # results land in out/results/
```

### `mro2nf` flags

| Flag | Meaning |
|---|---|
| `-o <dir>` | output directory for the Nextflow project (default `out`) |
| `-mropath <path>` | `@include` search path (`:`-separated); default `$MROPATH` when set, else the `.mro`'s directory, as `mrp`; as in `mrp`, the `.mro`'s own directory is searched first, for its own includes and its stages' code only, and the working directory never |
| `-confine-outputs` | fail a stage task whose output resolves outside its work dir instead of copying it in, for a project run under an isolating executor such as docker |
| `-version` | print the mro2nf version and exit |

The tasks run the `mre` on `PATH` where the pipeline runs (`make install` on
the host, the runtime image in a container), which runs py and comp stages
under the `mrjob` beside it and py stages with the Martian adapters it embeds.
A run names others with `--mro2nf_mre <path>`, `--mro2nf_mrjob <path>` and
`--mro2nf_adapters <dir>` (Martian's adapters dir, holding
`python/martian_shell.py`), such as a pipeline bundle's own Martian runtime.
`--mro2nf_monitor true` is mrp's `--monitor`: `mrjob` enforces each job's
memory and thread limits. A launch-time mrp `--overrides` file is tracked by
#301.

### Targets: one project, chosen with `-profile`

Every target runs the same generated project; `nextflow run -profile <name>`
chooses it:

| Profile | Runs on | Needs |
|---|---|---|
| `standard` (default), `slurm`, `sge`, `lsf`, `pbs` | the host's `mre` and code, a shared filesystem | — |
| `docker` | local docker, each task in its own container | `--mro2nf_image <image>` |
| `awsbatch` | AWS Batch over an S3 work dir | `--mro2nf_image <image in ECR>`, `-work-dir s3://…`, `process.queue` and `aws.region` in a `-c` config |
| `healthomics` | AWS HealthOmics | `./package.sh <image>@sha256:…`, which makes it the workflow's own config |

A container profile launched without `--mro2nf_image` fails before any task
runs. Entry file values are staged into the tasks that read them (`s3://`
URIs on every profile); see `docs/TRANSPILER.md` §8 and §9, and
`docs/LIVE_AWS_TEST.md` for the AWS runs.

### Container image

The container profiles run every task, PLAN, the stages and OUTS, in one
image: `--mro2nf_image`. Build it `FROM` mro2nf's runtime image, which puts
`mre` and the `mrjob` beside it on its `PATH` at `/opt/mro2nf`
(`deploy/runtime/Dockerfile`), and add your pipeline's code:

```dockerfile
FROM ghcr.io/eunmann/mro2nf-runtime:<version>
COPY my-pipeline/ /opt/pipeline/
```

Use the runtime tag of the mro2nf release that transpiles the project, so the
`mre` ABI matches; for a transpiler built from a clone, build the runtime
image from the same checkout (`docker build -f deploy/runtime/Dockerfile -t
mro2nf-runtime:local .`) and use that. Install anything else your stages run
in the same Dockerfile: a stage whose `src` names a command finds it on the
image's `PATH`.

A stage task finds the entry `.mro`, its includes and its stages' code at the
absolute paths the project was transpiled from, so transpile the pipeline
where it sits at the path the image holds it: with the tree at
`/opt/pipeline` on the transpiling host too, transpile the entry there, and
every relative stage `src` resolves below it. Equivalently, keep
the tree where it is and `COPY` it to that same path in the image (`COPY
my-pipeline/ /home/me/my-pipeline/`), as the docker e2e suite does.

```bash
docker build -t my/image:tag .
mro2nf -o out /opt/pipeline/mro/pipeline.mro
cd out
nextflow run main.nf -profile docker --mro2nf_image my/image:tag

# AWS Batch: push the image to ECR; batch.config sets process.queue and aws.region
nextflow run main.nf -profile awsbatch -c batch.config -work-dir s3://<bucket>/work \
    --mro2nf_image <account>.dkr.ecr.<region>.amazonaws.com/my-image:tag \
    --outdir s3://<bucket>/results
```

The runtime image itself serves as `--mro2nf_image` when the code is
reachable at those paths without copying it, such as a docker profile run
whose `-c` config mounts it (`docker.runOptions = '-v
/opt/pipeline:/opt/pipeline:ro'`).

### Run parameters

Entry-pipeline inputs are exposed as run parameters: each becomes `params.<name>`
(null keeps the transpiled `.mro`'s value), so you can override them at launch
with a Nextflow `-params-file` or on the command line — no re-transpile. The
run's PLAN task recompiles the invocation with them, so a launch value changes
forks, gates and pruned calls as an `.mro` literal would under `mrp`:

```bash
nextflow run main.nf -params-file inputs.json        # e.g. {"values": [5, 6, 7]}
nextflow run main.nf --n 5 --scale 2.5 --skip false  # scalars, parsed as declared
```

A file-typed input is a path the stages open as given, as under `mrp`: give it
as an absolute path every task can see. See `docs/TRANSPILER.md` §8.

## Testing

```bash
make test          # unit tests (in-process)
make cover         # unit-test coverage gate (fails below COVER_MIN%)
make test-e2e      # golden table + mrp differential + failure parity + knobs
make lint-nf       # nextflow lint of every fixture's project
make bench         # orchestration overhead at two widths per shape
make gen-diff      # generated programs vs real mrp (GEN_SEEDS=<start>|random GEN_COUNT=<n>)
make lint-check    # golangci-lint (no auto-fix)
```

The e2e suites are Go tests in `test/e2e` (build tag `e2e`, always run with
`-count=1` so the test cache can't return a stale ok). `make test-e2e` runs
each fixture's transpiled pipeline under Nextflow in a bounded parallel pool
(`E2E_PARALLEL=<n>`, default 10) and diffs the result against the committed
real-`mrp` golden; it also runs each fixture under a live `mrp` and diffs the
published tree and every stage's `_args`, `_outs` and `_jobinfo`, and covers
the retry/ASSERT failure contract and `-resume` cache stability.

### How it's validated

Every transpiled pipeline is diffed against the output of the **real `mrp`**
orchestrator, so "correct" means byte-identical to Martian:

- **Unit** (`make test`): the call graph, resolver, topology, plan, posted fork
  elements and gates, the type walk and bundles, the generated project, and a
  stand-in run of every fixture's call graph through the `mre` subcommands.
- **Local e2e** (`make test-e2e`): the golden fixture table — `.mro` fixtures
  transpiled, run under Nextflow, diffed vs committed `mrp` goldens — and the
  live `mrp` differential, which also compares the outcome of every fixture
  `mrp` fails, covering every supported feature.
- **Generative differential** (`TestGenDiff`; `make gen-diff` for long runs):
  random well-typed programs from `test/gen` — structs, typed maps, nested
  arrays, files, split stages, sub-pipelines, map calls of every kind, disable
  gates, projections, wildcards, fan-ins and boundary values — run under real
  `mrp` and under mro2nf + Nextflow and face the same output, stage-ABI and
  failure-parity checks as the fixtures. A divergence is shrunk to a minimal
  fixture with its `mrp` golden. `make test-e2e` runs a fixed smoke set of
  seeds.
- **Overhead** (`make bench`, nightly `make test-scale`): the plumbing task
  count must not change with fork width or chunk count, and the stage tasks
  must equal `mrp`'s job count.

## Running on a cluster

`nextflow.config` ships grid executor profiles; select one with `-profile`:

```bash
nextflow run main.nf -profile slurm          # or sge / lsf / pbs
```

The default `standard` profile runs locally, clamping each task's request to
the host's cpus and memory. A stage's `special` key selects
`params.job_resources[special]` as the task's `clusterOptions`.

## Feature coverage & limitations

`docs/FEATURE_COVERAGE.md` is the authoritative matrix. It comes from a
page-by-page read of all seven martian-lang.org docs, cross-checked against the
`martian/syntax` grammar, and maps every Martian feature to its support status
and the test that exercises it.

Supported: stages, pipelines, and calls; split/main/join; map calls over arrays,
maps, split stages, and sub-pipelines, nested and zipped; disabled calls and
map calls, decided per fork; structs and typed maps (including `map<S>.field`
projection); file, array, map, and struct-of-file outputs; the `py`, `exec`,
and `comp` adapters; dynamic per-chunk resources; content-based retry (only
the failures mrp retries, a signal, ENOMEM or a scheduler failure, retry, with
escalated memory; an ASSERT or a stage exception ends the run); and mrp's
`--monitor` (`--mro2nf_monitor true`).

Resources track mrp, so a transpiled run schedules and finishes comparably.
Per-chunk `cpus` and `memory`, the split-returned `join` override, and the
`special` scheduler key all reach the executor, and every job reports the
allocation mrp would give it. The reserved `special = "gpu"` / `"gpu:N"` also
requests N GPUs via the `accelerator` directive, for every job whose special it
is — see [`docs/GPU.md`](docs/GPU.md). A few things diverge in timing or
mechanism but never in output: there is no mid-run VDR deletion (work dirs are
retained), and the published `outs/` tree is copied into place (mrp moves +
symlinks) — the tree's layout and contents match mrp's.

## Development

`make help` lists all targets. House rules live in `CLAUDE.md` and
`.claude/skills/`. In short: no behavior changes without tests, functions ≤ 70
lines, wrap errors with `%w`, standard `testing` + `go-cmp` only, and
`make lint-check` must pass.

CI (`.github/workflows/pr-validation.yml`) runs lint (golangci-lint + `govulncheck`
+ a `go mod tidy` check), build, unit tests (`-race`), and the e2e suites on every
PR. It doesn't run again on `main`: a PR merges only when it is up to date with
`main`, so its run already validated the merged tree. `nightly.yml` reruns the e2e
suites daily on the declared Nextflow floor and on latest stable, with a
1,000-fork `make test-scale` smoke. Weekly, or on a dispatch with `heavy`, it
runs the full `make test-scale` scale tier and a long `make gen-diff` run from a
random seed, whose shrunk repros it keeps as an artifact. A scheduled run is
skipped when the current `main` commit and the latest-stable Nextflow release
have already passed its tier. A failed run opens,
or comments on, a "Nightly CI is failing" issue. Gen-diff divergences don't fail
the run: they go to a "Generative differential: new divergences" issue. The Martian
dependency is pinned to a published commit, so
CI needs only a single checkout; to hack on the Martian parser locally, add a
`go.work` pointing at a fork checkout (gitignored). Tagging `v*` triggers
`release.yml`. It requires the tagged commit to be a PR's merge whose tree matches
that PR's head, and that head to have a green PR Validation run. It then cross-compiles the `mro2nf` and `mre` binaries and publishes them in a
GitHub release with a `SHA256SUMS` file, `LICENSE` and `THIRD_PARTY_NOTICES`. It also pushes the runtime image. A
pre-release tag (`v1.2.0-rc1`) never becomes the latest release or the image's
`latest` tag. In a public repository the binaries and the image carry build
provenance; check a download with `sha256sum -c SHA256SUMS --ignore-missing` and
`gh attestation verify <file> --repo eunmann/mro2nf`.

## License

MIT — see [`LICENSE`](LICENSE). The binaries and the runtime image also carry
third-party code, listed with its licenses in
[`THIRD_PARTY_NOTICES`](THIRD_PARTY_NOTICES) (`make notices` regenerates it). The
vendored Martian adapters keep Martian's license in
[`vendor-martian/LICENSE`](vendor-martian/LICENSE).
