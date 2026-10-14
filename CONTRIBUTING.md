# Contributing

House rules live in [`CLAUDE.md`](CLAUDE.md) and `.claude/skills/`. In short:
no behavior change without tests, functions of at most 70 lines, errors
wrapped with `%w`, the standard `testing` package and `go-cmp` only, and
`make lint-check` must pass. Changes land through pull requests.
[`docs/TRANSPILER.md`](docs/TRANSPILER.md) explains the design end to end.

## Layout

```
cmd/mro2nf/    transpiler CLI: .mro -> Nextflow project
cmd/mre/       runtime shim the generated tasks call:
               plan | stage | outs
internal/
  cg/          Martian's compiled call graph: witness, topology, plan, resolver
  cgnf/        call graph -> Nextflow project (main.nf wiring, stage.nf module)
  job/         one stage job through mrjob, as mrp runs it
  types/       type-directed file-leaf walk over Martian's compiled types
  shim/        bundle I/O, markers, output confinement
  logging/     slog setup
vendor-martian/python/   pinned martian_shell.py + martian.py adapters, which mre embeds (Martian's LICENSE beside)
THIRD_PARTY_NOTICES      licenses of the code the binaries link and the images vendor
testdata/      .mro fixtures with expected mrp outputs (e2e goldens; see testdata/README.md)
bench/         the overhead benchmark fixtures and baseline
test/e2e/      Go e2e suites (golden diff vs mrp, mrp differential, docker, bench, scale)
test/divergence/   the divergence allowlist and the FEATURE_COVERAGE table generator
test/gen/      generator of random well-typed programs for the generative differential
test/scale/    the scale tier's gates
test/wide/     a synthetic pipeline of many stage calls
test/thirdparty/   THIRD_PARTY_NOTICES generator, vendored-license and vendor-martian drift checks
scripts/aws/   normcmp.py, the path-normalized outs comparison of live AWS runs
scripts/aws/perf/   the critical-path and overhead analysis of an AWS Batch run
deploy/        the runtime image; a minimal AWS CDK (S3 + ECR + Batch + HealthOmics role)
docs/          user and design docs
.github/workflows/   PR validation, nightly, CDK and release CI
```

The Martian parser is a normal module dependency, pinned by commit. To work on
the parser locally, add a gitignored `go.work` pointing at a checkout.

## Building and testing

`make help` lists every target.

```bash
make build         # ./mro2nf, ./mre and ./mrjob (the pinned Martian's)
make test          # unit tests
make cover         # unit-test coverage gate (fails below COVER_MIN%)
make lint-check    # golangci-lint, no auto-fix
make test-e2e      # golden table, mrp differential, failure parity, cloud-sim, knobs
make test-e2e-docker   # pipelines under the docker profile, in images built FROM the runtime image
make test-mrp-diff # the mrp differential alone
make lint-nf       # nextflow lint of every fixture's project
make bench         # orchestration overhead at two widths per shape
make test-scale    # the nightly scale tier
make gen-diff      # generated programs vs real mrp (GEN_SEEDS=<start>|random GEN_COUNT=<n>)
make docs-divergences  # regenerate docs/FEATURE_COVERAGE.md's divergence table
```

The e2e suites are Go tests in `test/e2e` (build tag `e2e`, always run with
`-count=1` so the test cache cannot return a stale pass). `make test-e2e` runs
each fixture's transpiled pipeline under Nextflow in a bounded parallel pool
(`E2E_PARALLEL=<n>`, default 10) and diffs the result against the committed
`mrp` golden. It also runs each fixture under a live `mrp` and diffs the
published tree and every stage's `_args`, `_outs` and `_jobinfo`, and covers
the retry and `ASSERT` failure contract and `-resume` stability. The `mrp`
suites need a Martian build at `MARTIAN_BIN` (default `~/workdir/martian/bin`)
and skip without one.

### How it's validated

Each transpiled pipeline is compared with the output of the real `mrp`.
Every difference must match an entry of the divergence allowlist,
`test/e2e/divergences.json`:

- **Unit** (`make test`): the call graph, resolver, topology, plan, posted fork
  elements and gates, the type walk and bundles, the generated project, and a
  stand-in run of every fixture's call graph through the `mre` subcommands.
- **Local e2e** (`make test-e2e`): the golden fixture table, and the live `mrp`
  differential, which also compares the outcome of every fixture `mrp` fails.
  Every fixture faces live `mrp` except the two `mrp` cannot complete
  (`mrpUnrunnable` in `test/e2e/coverage_test.go`).
- **Generative differential** (`TestGenDiff`; `make gen-diff` for long runs):
  random well-typed programs from `test/gen` run under real `mrp` and under
  mro2nf and face the same output, stage-ABI and failure-parity checks as the
  fixtures. A divergence is shrunk to a minimal fixture with its `mrp` golden.
  `make test-e2e` runs a fixed smoke set of seeds.
- **Overhead** (`make bench`, nightly `make test-scale`): the plumbing task
  count must not change with fork width or chunk count, and the stage tasks
  must equal `mrp`'s job count. [`docs/BENCHMARKS.md`](docs/BENCHMARKS.md)
  describes the metrics and gates.
- **CellRanger** (`make test-cellranger`, opt-in): the real CellRanger `count`
  pipeline under both runners;
  [`docs/CELLRANGER_LOCAL.md`](docs/CELLRANGER_LOCAL.md).

## CI

`.github/workflows/pr-validation.yml` runs lint (golangci-lint, `govulncheck`
and a `go mod tidy` check), build, unit tests (`-race`), and the e2e suites on
every PR. It does not run again on `main`: a PR merges only when it is up to
date with `main`, so its run already validated the merged tree.

`nightly.yml` reruns the e2e suites daily on the declared Nextflow floor and on
the latest stable release, with a 1,000-fork `make test-scale` smoke. Weekly,
or on a dispatch with `heavy`, it runs the full scale tier and a long
`make gen-diff` run from a random seed, whose shrunk repros it keeps as an
artifact. A scheduled run is skipped when the current `main` commit and the
latest stable Nextflow have already passed its tier. A failed run opens, or
comments on, a "Nightly CI is failing" issue. Generative-differential
divergences do not fail the run: they go to a "Generative differential: new
divergences" issue.

## Releases

Tagging `v*` triggers `release.yml`. It requires the tagged commit to be a
PR's merge whose tree matches that PR's head, and that head to have a green PR
Validation run. It then cross-compiles `mro2nf`, `mre` and `mrjob` for Linux
and macOS (amd64, arm64) and publishes a GitHub release with one archive per
platform, `mro2nf-<tag>-<os>-<arch>.tar.gz`, and a `SHA256SUMS` file covering
the archives. Each archive holds a single directory of the same name with
`mro2nf`, `mre` and `mrjob` under those names, as `mre` runs the `mrjob`
beside it, plus `LICENSE` and `THIRD_PARTY_NOTICES`. The archives are
reproducible: entries sorted by name, owned and writable only by root, with
the tagged commit's time, and gzip without a name or timestamp
(`test/e2e/workflow_release_test.go` runs the packaging step). It also pushes
the runtime image `ghcr.io/eunmann/mro2nf-runtime:<tag>` for linux/amd64 and
linux/arm64. A pre-release tag (`v1.2.0-rc1`) never becomes the latest release
or the image's `latest` tag. In a public repository the archives and the image
carry build
provenance; check a download with `sha256sum -c SHA256SUMS --ignore-missing`
and `gh attestation verify <file> --repo eunmann/mro2nf`.
