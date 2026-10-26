# Contributing

## House rules

The full rules live in [`CLAUDE.md`](CLAUDE.md) and `.claude/skills/`. In short:

- No behavior change without tests.
- Functions of at most 70 lines; errors wrapped with `%w`.
- The standard `testing` package and `go-cmp` only.
- `make lint-check` must pass. Fix lint by improving the code, not with
  `//nolint`.
- Changes land through pull requests, as atomic commits, after PR Validation
  goes green.

[Architecture](docs/README.md#architecture) explains the design end to end;
read the [overview](docs/architecture/overview.md) first.

## Quick start

```bash
make build         # ./mro2nf, ./mre and ./mrjob
make test          # unit tests
make lint-check    # golangci-lint, no auto-fix
make test-e2e      # the e2e suites: needs Nextflow, Java and a Martian build at MARTIAN_BIN
```

The Martian parser is a normal module dependency, pinned by commit. To work on
the parser locally, add a gitignored `go.work` pointing at a checkout.

## Layout

```
cmd/mro2nf/          the transpiler CLI: .mro -> Nextflow project
cmd/mre/             the runtime shim the generated tasks call: plan | stage | outs
internal/
  cg/                Martian's compiled call graph: witness, topology, plan, resolver
  mrcore/            Martian core's runtime resolution, ported to read staged records
  cgnf/              call graph -> Nextflow project (main.nf wiring, stage.nf module)
  job/               one stage job through mrjob, as mrp runs it
  types/             the type-directed file-leaf walk over Martian's compiled types
  shim/              bundle I/O, markers, output confinement
vendor-martian/python/   the pinned Martian adapters, which mre embeds (Martian's LICENSE beside)
THIRD_PARTY_NOTICES      licenses of the code the binaries link and the images vendor
testdata/            .mro fixtures with expected mrp outputs (see testdata/README.md)
bench/               the overhead benchmark fixtures and baseline
test/e2e/            Go e2e suites (golden diff vs mrp, mrp differential, docker, bench, scale)
test/divergence/     the divergence allowlist and the divergence table generator
test/gen/            generator of random well-typed programs for the generative differential
test/scale/          the scale tier's gates
test/wide/           a synthetic pipeline of many stage calls
test/thirdparty/     THIRD_PARTY_NOTICES generator, vendored-license and vendor-martian drift checks
scripts/aws/         normcmp.py, the path-normalized outs comparison of live AWS runs
scripts/aws/perf/    the critical-path and overhead analysis of an AWS Batch run
deploy/              the runtime image; a minimal AWS CDK app (S3 + ECR + Batch + HealthOmics role)
docs/                user, reference, architecture and development docs
.github/workflows/   PR validation, nightly, CDK and release CI
```

## Development docs

| Page | Covers |
|---|---|
| [Testing](docs/development/testing.md) | Make targets, the test tiers, how mro2nf is validated against `mrp`, the Cell Ranger check |
| [Golden fixtures](testdata/README.md) | The fixtures and how their goldens are recorded |
| [Benchmarks](docs/development/benchmarks.md) | The overhead benchmark and the nightly scale tier |
| [CI and releases](docs/development/ci-and-releases.md) | The workflows and how to cut a release |
| [Divergences](docs/reference/divergences.md) | Every known difference from `mrp`, and how the allowlist works |
