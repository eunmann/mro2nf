# mro2nf documentation

Start with [Getting started](getting-started.md), which installs mro2nf and
runs a first pipeline. The rest is grouped by what you are doing.

## User guide

For anyone transpiling and running a Martian pipeline.

| Page | Covers |
|---|---|
| [Getting started](getting-started.md) | Install, a first run, the outputs, `-resume` |
| [Transpiling](user-guide/transpiling.md) | The `mro2nf` command, the generated project, where the code must stay |
| [Parameters](user-guide/parameters.md) | Pipeline inputs at launch, the parameter schema, the `--mro2nf_*` options |
| [Files and paths](user-guide/files-and-paths.md) | How file inputs reach a stage on each profile, companion files, paths written into text |
| [Resources and retries](user-guide/resources-and-retries.md) | CPU and memory per job, `mrp --overrides`, `withName:`, retries, GPUs |
| [Profiles](user-guide/profiles.md) | Local, grid, Docker, AWS Batch, AWS HealthOmics |
| [Container images](user-guide/container-images.md) | The runtime image, building a pipeline's image, CUDA images, vendor bundles such as Cell Ranger |
| [`mrp` equivalents](user-guide/mrp-equivalents.md) | Each `mrp` flag and what to use instead |
| [Troubleshooting](user-guide/troubleshooting.md) | Exit codes, a task's work directory, error messages |

## Reference

| Page | Covers |
|---|---|
| [Feature coverage](reference/feature-coverage.md) | Every Martian feature, its support status and the tests that cover it |
| [Divergences](reference/divergences.md) | Every known difference from `mrp`, generated from the allowlist the tests apply |
| [AWS validation](reference/aws-validation.md) | Live runs on AWS Batch and HealthOmics, and Cell Ranger's performance on Batch |

## Architecture

For anyone changing mro2nf or wanting to know how it works. Read them in
order.

| Page | Covers |
|---|---|
| [Overview](architecture/overview.md) | The requirements, the two contracts, who owns what, the Martian code reused |
| [Concepts](architecture/concepts.md) | The data structures and algorithms, with links to the code |
| [Transpile time](architecture/transpile.md) | The witness call graph, the topology, the generated project |
| [Run time](architecture/runtime.md) | PLAN, instances, gates, reads, split stages, OUTS, caching |
| [The `mre` shim](architecture/mre.md) | Each `mre` subcommand and the steps of a stage task |
| [Bundles and file flow](architecture/bundles.md) | How files move between tasks: bundles, markers, origins, references |
| [Inputs](architecture/inputs.md) | How launch values and entry files become the run's invocation |
| [Targets](architecture/targets.md) | The profiles, the runtime image, resource grants |

## Development

| Page | Covers |
|---|---|
| [Contributing](../CONTRIBUTING.md) | House rules and the repository layout |
| [Testing](development/testing.md) | Make targets, the test suites and how mro2nf is validated against `mrp` |
| [Golden fixtures](../testdata/README.md) | The fixtures and how their goldens are recorded |
| [Benchmarks](development/benchmarks.md) | The overhead benchmark and the nightly scale tier |
| [CI and releases](development/ci-and-releases.md) | The workflows and how to cut a release |
| [AWS Batch analysis scripts](../scripts/aws/perf/README.md) | Measuring a Batch run's critical path |
| [AWS test infrastructure](../deploy/awsbatch-cdk/README.md) | The example CDK stack for AWS Batch and HealthOmics |
