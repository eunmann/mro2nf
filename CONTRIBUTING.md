# Contributing

The rules are in [`CLAUDE.md`](CLAUDE.md) and `.claude/skills/`. In short: no
behavior change without a test; functions of at most 70 lines; errors wrapped
with `%w`; the standard `testing` package and `go-cmp` only; fix lint by
improving the code, not with `//nolint`. Changes land as atomic commits through
a pull request once PR Validation is green.

[How mro2nf works](docs/design.md) explains the design.

## Setup

You need Linux, Go 1.27, `make`, and for the e2e suites Nextflow 26.04 or later,
Java 17 to 26, Python 3 and Docker. The `mrp` suites also need a Martian build
at `MARTIAN_BIN` (default `~/workdir/martian/bin`) from the commit `go.mod`
pins; they skip without one.

The Martian parser is a normal module dependency, pinned by commit. To work on
it, add a gitignored `go.work` pointing at a checkout. To move the pin, follow
"Bumping the Martian Pin" in
[`.claude/skills/workflow/SKILL.md`](.claude/skills/workflow/SKILL.md).

## Layout

```
cmd/mro2nf/            the transpiler CLI
cmd/mre/               the runtime shim: plan | stage | outs
internal/              cg, mrcore, cgnf, job, types, shim, fileio, buildinfo (docs/design.md)
vendor-martian/python/ the pinned Martian adapters, which mre embeds
testdata/              .mro fixtures with expected mrp outputs (testdata/README.md)
bench/                 the overhead benchmark fixtures and baseline
test/e2e/              e2e suites: goldens, mrp differential, docker, bench, scale
test/divergence/       the divergence allowlist checks and table generator
test/gen/              random well-typed programs for the generative differential
test/corpus/           invocations of the real-world .mro corpus (testdata/corpus, MRO2NF_CORPUS)
test/scale/, test/wide/   the scale tier's gates; a pipeline of many stage calls
test/docs/             the Markdown link check
test/thirdparty/       THIRD_PARTY_NOTICES and drift checks against the Martian module
deploy/runtime/        the runtime image
deploy/awsbatch-cdk/   an example AWS stack (S3, ECR, Batch, HealthOmics role)
scripts/aws/           comparison and critical-path analysis of live AWS runs
```

## Make targets

`make help` lists them all.

| Target | Does |
|---|---|
| `make build`, `make install` | build `mro2nf`, `mre` and `mrjob`; install them into `PREFIX/bin` |
| `make test` | unit tests, the doc link check included |
| `make lint`, `make lint-check` | golangci-lint with and without `--fix` |
| `make cover` | the unit coverage gate (`COVER_MIN`) |
| `make test-e2e` | the e2e suites below, but docker, `nextflow lint`, bench, scale and Cell Ranger |
| `make test-e2e-docker` | pipelines under the `docker` profile, in images built from the runtime image |
| `make test-mrp-diff` | the `mrp` differential alone |
| `make lint-nf` | `nextflow lint` of every fixture's and corpus invocation's project |
| `make bench` | the overhead gates; `BENCH_UPDATE=1` records `bench/baseline.json` |
| `make test-scale` | the bench shapes at production widths (`SCALE_RUN`, `SCALE_WIDTH`) |
| `make gen-diff` | generated programs against `mrp` (`GEN_SEEDS`, `GEN_COUNT`) |
| `make test-cellranger` | the Cell Ranger check (`CELLRANGER_HOME`) |
| `make test-corpus` | the corpus sweeps, `nextflow lint` and the stub differential over the pipelines whose MROPATH is `CORPUS` |
| `make record-goldens` | re-record `testdata/*/expected/outs/` from `mrp` |
| `make docs-divergences`, `make notices` | regenerate `docs/divergences.md` and `THIRD_PARTY_NOTICES` |
| `make vulncheck`, `make deadcode`, `make cross` | govulncheck, unreachable code, a build for each release platform |

## Tests

The bar is `mrp` itself. Every difference a test finds must match an entry of
[`test/e2e/divergences.json`](test/e2e/divergences.json), or it fails.

| Tier | Command | Checks |
|---|---|---|
| Unit | `make test` | the call graph, resolver, topology, plan, posts, bundles, the emitter, and a stand-in run of every fixture through the `mre` subcommands |
| Goldens | `make test-e2e` (`TestGolden`) | every fixture transpiled, run under Nextflow, and compared with its committed `mrp` output |
| `mrp` differential | `make test-mrp-diff` | live `mrp` against Nextflow: the published tree, `pipeline_outs.json`, every stage's `_args`, `_outs` and `_jobinfo`, and failure parity; `TestMrpCallGraph` replays real pipestances through `mre` |
| Generative | `make gen-diff` | random programs from `test/gen` under both runners; a difference is shrunk to a minimal fixture |
| Docker | `make test-e2e-docker` | container isolation, the AWS Batch profile run on local Docker, the HealthOmics package |
| Overhead | `make bench`, `make test-scale` | tasks, staged bytes and driver heap at two widths per shape ([design](docs/design.md#the-overhead-rule)) |
| Cell Ranger | `make test-cellranger` | Cell Ranger 10.1.0 `testrun` under `mrp` twice and under Nextflow locally and in the cloud simulation: the published tree byte for byte (each file `mrp` itself varies run to run by its stable part) and one stage task per `mrp` job; needs a licensed bundle, never in CI |
| Corpus | `make test-corpus CORPUS=<mropath>` | every pipeline and stage under the MROPATH, invoked with its inputs unset, through Martian's compiler, the unit tests' call-graph sweeps and `nextflow lint`, then under both runners with every stage replaced by the generative differential's stub, under `CORPUS_SEEDS` seeds (`TestCorpusStubDiff`). The MIT corpus in `testdata/corpus` runs in `make test`, `make lint-nf` and `make test-e2e`, the AGPL repos in the nightly. Point it at your own licensed pipelines, such as a Cell Ranger install's `mro` dir; they never run in CI |

e2e tests carry the `e2e` build tag and always run with `-count=1`. A fixture
`mrp` cannot complete (`exec_min`, `mixed_adapters`) has a golden only.
`testdata/README.md` explains how goldens are recorded.

### Live AWS runs

No CI job runs on AWS. The last live runs are summarized in
[Compatibility](docs/usage.md#compatibility); their raw data is in the
example stack's private bucket under `archive/2026-10-aws-validation/`.
`scripts/aws/normcmp.py` compares an `mrp` `_outs` with a run's
`pipeline_outs.json`, and [`scripts/aws/perf`](scripts/aws/perf/README.md)
analyzes a Batch run's critical path. The Batch fixture matrix ran 40
fixtures, among them `file_tree`, `split_forward_file`, `shared_out_file`,
`map_call_entry`, `gate_chain`, `map_file_2d`, `forkdisabled`,
`entry_filearr` and `entry_dir`, each with its `results/` compared with a
local run. Add `dir_hidden` (empty directories and dotfiles over S3, #624) on
the next live run.

## CI

| Workflow | Runs | Gates |
|---|---|---|
| [`pr-validation.yml`](.github/workflows/pr-validation.yml) | every PR | lint, `go mod tidy`, `make vulncheck` and `make deadcode`; `make cross`; unit tests with `-race` and coverage; the e2e and docker suites; `make lint-nf`; `make bench` |
| [`nightly.yml`](.github/workflows/nightly.yml) | daily; weekly for the heavy tier | the e2e suites on Nextflow 26.04.0 and the latest stable, a 1,000-fork scale smoke, `make test-corpus` on the AGPL pipeline repos at pinned commits, unit tests, lint, vulnerability scans of the code and the runtime image; weekly, the full scale tier and a long `make gen-diff` |
| [`cdk-validation.yml`](.github/workflows/cdk-validation.yml) | changes to `deploy/awsbatch-cdk` | compiles and synthesizes the example stack |
| [`release.yml`](.github/workflows/release.yml) | dispatched by hand | cuts a release |

A PR that changes only Markdown, `docs/`, `.claude/` or `LICENSE` skips the
code jobs and runs only the tests that read those files. PR Validation does not
run again on `main`: a PR merges only when up to date with it. A failed nightly
opens or comments on a "Nightly CI is failing" issue.

## Releases

Write the notes to `notes.md`, then dispatch:

```bash
gh workflow run release.yml -f version=v0.7.0 -f ref=main -F notes=@notes.md
```

The workflow requires the commit to be a merged PR with a green PR Validation.
It builds the archives for linux/amd64 and linux/arm64 and the multi-arch
runtime image, scans the image with Grype, and smoke-tests each on its
platform. Only then does it tag, push `ghcr.io/eunmann/mro2nf-runtime:<version>`
(and `latest` for a non-pre-release), and publish the GitHub release with the
archives, `SHA256SUMS`, the notes, and GitHub's list of merged PRs. A failure
before publishing leaves nothing behind; if publishing fails partway, delete
what it created and dispatch again. `-f dry_run=true` builds everything and
publishes nothing.

The archives are reproducible: sorted entries, root ownership, the commit's
time, and gzip without a name or timestamp. Releases after v0.6.0 carry build
provenance (`gh attestation verify <file> --repo eunmann/mro2nf`, or
`oci://ghcr.io/eunmann/mro2nf-runtime:<version>` for the image). v0.6.0 and
earlier have none: `SHA256SUMS` is their only check.

There is no changelog file; each
[GitHub release](https://github.com/eunmann/mro2nf/releases) lists its changes.
