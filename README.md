# mro2nf

mro2nf transpiles a [Martian](https://martian-lang.org) pipeline (`.mro`) into a
native [Nextflow](https://www.nextflow.io) project. The project runs your stages
in `mrp`'s order, with `mrp`'s arguments and its cpu and memory sizing. Each
stage task runs your original stage code, unchanged, through `mrjob` and the
Martian adapter, as `mrp` runs it.

A transpiled pipeline publishes the same outputs as `mrp`, except for the
divergences listed in
[`docs/FEATURE_COVERAGE.md`](docs/FEATURE_COVERAGE.md#divergences-from-mrp).
For example, `pipeline_outs.json` names files by their paths relative to
`results/`, and a published symlink becomes a copy of its target. That
document also lists every Martian feature and its support status.

## Prerequisites

- Go 1.26 and `make`, to build. The build downloads Go modules.
- Java 17 to 25, and Nextflow 25.10 or later (`curl -s https://get.nextflow.io | bash`).
  The generated `nextflow.config` sets `manifest.nextflowVersion '!>=25.10.0'`,
  so an older Nextflow refuses to run it.
- Python 3 on the host, to run `py` stages under the local and grid profiles.
- Docker, for the container profiles.

## Install

```bash
git clone https://github.com/eunmann/mro2nf
cd mro2nf
make install                          # mro2nf, mre and mrjob into ~/.local/bin
export PATH="$HOME/.local/bin:$PATH"
mro2nf -version
```

`make install PREFIX=<dir>` installs into `<dir>/bin` instead. Keep `mre` and
`mrjob` in the same directory: `mre` runs the `mrjob` beside it.

## First run

From the clone, transpile the `testdata/split_test` fixture and run it with
the array input `values = [5, 6, 7]`:

```bash
mro2nf -o out testdata/split_test/pipeline.mro
cd out
echo '{"values": [5, 6, 7]}' > inputs.json
nextflow run main.nf -params-file inputs.json
cat results/pipeline_outs.json
```

The run succeeds with 8 tasks: one PLAN task, the `SUM_SQUARES` split, its
three chunks and its join, `REPORT`, and the OUTS task that publishes the
results. The output is:

```json
{
    "sum": 110
}
```

Each entry input of the pipeline is a run parameter. Without one, the run uses
the value in the `.mro`'s `call`. A scalar can be passed on the command line
(`--n 5`). An array, map or struct must be passed as typed JSON in a
`-params-file`. Give file inputs as absolute paths.

The generated project refers to the entry `.mro`, its includes and its stage
code by the absolute paths they had at transpile time. Don't move or delete
them; if you do, transpile again.

## Outputs

- `results/` holds the published outputs: `mrp`'s `outs/` tree and
  `pipeline_outs.json`. `--outdir <dir>` publishes them elsewhere, including
  to an `s3://` prefix.
- `work/` holds one directory per task, with the stage's metadata and logs.
- `.nextflow/`, `.nextflow.log` and `trace-<timestamp>.txt` are Nextflow's
  run state, log and task trace, written to the launch directory.

A failed run publishes nothing to `results/`.

## Resume

```bash
nextflow run main.nf -params-file inputs.json -resume
```

A rerun with nothing changed takes all 8 tasks from the cache and runs none.
A changed entry parameter re-runs every
task. Nextflow does not see edits to stage code, so after you change stage
code, run without `-resume`.

## Container image

The `docker`, `awsbatch` and `healthomics` profiles run every task in one
image, which you name with `--mro2nf_image`. Build it `FROM` the mro2nf runtime
image, which has `mre`, `mrjob` and the Martian adapters at `/opt/mro2nf` on
its `PATH`, and add your pipeline's code.

The runtime image is `ghcr.io/eunmann/mro2nf-runtime:<version>`, where
`<version>` is the release tag of the mro2nf that transpiled the project (what
`mro2nf -version` prints for a release build). For a build of an unreleased
commit, build the runtime image from the same checkout:

```bash
docker build -f deploy/runtime/Dockerfile -t mro2nf-runtime:local .
```

A task looks for the pipeline's code, the entry `.mro` included, at the
absolute path it was transpiled from, so copy the code to that same path in the
image. For the first run above,
from the root of the clone:

```bash
cat > split_test.Dockerfile <<EOF
FROM mro2nf-runtime:local
COPY . $PWD/testdata/split_test/
EOF
docker build -f split_test.Dockerfile -t split-test:local testdata/split_test
cd out
nextflow run main.nf -profile docker --mro2nf_image split-test:local -params-file inputs.json
```

The build context, `testdata/split_test`, is the directory the code is in, and
`COPY` writes it to the path `mro2nf` read it from. Install anything else your
stages need in the same Dockerfile, such as Python packages, or the program
that a stage whose `src` names a command runs, on the image's `PATH`.

## More

- [`docs/RUNNING.md`](docs/RUNNING.md): launch parameters, resources and
  overrides, retries, the profiles (grid, AWS Batch, HealthOmics), GPU stages,
  `mrp` flags and their Nextflow equivalents, and troubleshooting.
- [`docs/FEATURE_COVERAGE.md`](docs/FEATURE_COVERAGE.md): Martian features,
  their support, and the divergences from `mrp`.
- [`docs/TRANSPILER.md`](docs/TRANSPILER.md): how the transpiler and the
  generated project work.
- [`CONTRIBUTING.md`](CONTRIBUTING.md): building, testing, CI and releases.

## License

MIT — see [`LICENSE`](LICENSE). The binaries and the runtime image also carry
third-party code, listed with its licenses in
[`THIRD_PARTY_NOTICES`](THIRD_PARTY_NOTICES) (`make notices` regenerates it). The
vendored Martian adapters keep Martian's license in
[`vendor-martian/LICENSE`](vendor-martian/LICENSE).
