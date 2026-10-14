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

A release is cut by dispatching `release.yml`, which creates the tag itself;
pushing a tag by hand releases nothing. Write the curated notes, such as
breaking changes and highlights, in Markdown to `notes.md`, then run:

```bash
gh workflow run release.yml -f version=v0.6.0 -f ref=main -F notes=@notes.md
```

`-F notes=@notes.md` passes the file's contents, so multi-line Markdown and
its headings arrive as written. The inputs are:

- `version` (required): `v<major>.<minor>.<patch>`, optionally with a
  `-<prerelease>` suffix of dot-separated identifiers (`v0.6.0-rc1`,
  `v0.6.0-rc.1`) that git takes in a tag name. Its tag must not exist yet.
- `ref` (default `main`): the branch, tag or commit SHA to release, resolved
  to a commit.
- `notes` (optional): the curated Markdown.
- `dry_run` (default `false`): build everything, publish and tag nothing.

The Actions tab's "Run workflow" form offers the same inputs, but its notes
field holds a single line: leave it empty and edit the release's text once it
is published.

The workflow first requires the commit to be a PR's merge whose tree matches
that PR's head, and that head to have a green PR Validation run, and refuses
to run from any branch but `main`, since a dispatch runs the `release.yml` of
the branch it names (`--ref`, by default `main`). It then
cross-compiles `mro2nf`, `mre` and `mrjob` for Linux and macOS (amd64,
arm64), packs one archive per platform, and builds the runtime image for
linux/amd64 and linux/arm64 without pushing it. Only once every build
succeeds does it publish, in this order:

1. the annotated tag `<version>` on the commit, tagged by
   `github-actions[bot]`, whose message is the notes as written;
2. the runtime image `ghcr.io/eunmann/mro2nf-runtime:<version>`, also tagged
   `latest` unless the version is a pre-release, the exact image built;
3. the GitHub release on that tag, titled with it, with one archive per
   platform, `mro2nf-<version>-<os>-<arch>.tar.gz`, and a `SHA256SUMS` file
   covering the archives. Its body is the notes above the list of PRs merged
   since the previous tag that GitHub generates, grouped by
   `.github/release.yml`; without notes it is just that list.

A failed build therefore leaves no tag, image or release. If publishing itself
fails partway, delete whatever it created, the tag included, and dispatch
again. A pre-release is never the latest release.

Each archive holds a single directory of the same name with `mro2nf`, `mre`
and `mrjob` under those names, as `mre` runs the `mrjob` beside it, plus
`LICENSE` and `THIRD_PARTY_NOTICES`. The archives are reproducible: entries
sorted by name, owned and writable only by root, with the released commit's
time, and gzip without a name or timestamp
(`test/e2e/workflow_release_test.go` runs the packaging step). In a public
repository the archives and the image carry build provenance; check a download
with `sha256sum -c SHA256SUMS --ignore-missing` and `gh attestation verify
<file> --repo eunmann/mro2nf`.

To check the pipeline without releasing, for example after an action bump on
a branch, add `-f dry_run=true`; `--ref` runs that branch's `release.yml`:

```bash
gh workflow run release.yml --ref <branch> -f version=v0.0.0-dryrun -f ref=<branch> -f dry_run=true
```

A dry run validates the inputs, the version's tag included, so it needs a
version never released, and builds the archives and the runtime image, but
skips the PR Validation and `main` requirements and tags and publishes
nothing.
