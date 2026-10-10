# Golden fixtures

Each directory is one pipeline fixture: `pipeline.mro`, its `stages/<stage>/`
code, and an `expected/` tree holding the **goldens** the transpiled Nextflow
run must reproduce. A golden has two parts:

- **The outputs JSON** (`expected/outs.json`, or the file the `goldenCases`
  entry in `test/e2e/golden_test.go` names): mrp's path-normalized pipeline
  `_outs`. It is copied in by hand from an mrp run, so its provenance is a
  convention, not a mechanism. Fixtures mrp cannot run (`jsonOnlyGoldens` in
  `test/e2e/goldentree_test.go`, e.g. `exec_min`, `mixed_adapters`) have JSON
  goldens that no mrp run produced.
- **The published tree** (`expected/outs/`): every file mrp published under the
  pipestance's `outs/`, byte for byte, minus mrp's own root metadata (`_outs`,
  `manifest.json.gz`). Only `make record-goldens` writes it (see below). A
  fixture without `expected/outs/` published nothing under mrp.

## What the suites check

- **`TestGolden`** and **`TestDockerIsolation`** transpile the fixture, run it
  under Nextflow (the latter under container isolation), and require:
  - `results/pipeline_outs.json` to equal the outputs JSON; and
  - `results/`, minus `pipeline_outs.json` and `manifest.json.gz` at its root,
    to equal `expected/outs/`: no file missing, no file extra, every file's
    sha256 equal, and the same empty directories.

  Cases in `jsonOnlyGoldens` (fixtures mrp cannot run, and the `*_override`
  cases, which rerun a fixture with a launch-time `-params-file` mrp has no
  equivalent for) skip the tree check, each with its reason. These suites need
  no `mrp`, so they compare against a frozen snapshot.
- **`TestMrpDiff`** re-derives the truth **live**: it runs `mrp` and the
  transpiled Nextflow project on the same invocation and diffs both the
  published trees (same comparison as above) and the normalized `_outs`.
  Fixtures in its `cases` therefore machine-check their outputs JSON against
  mrp on every run, and, where a `native` / `runner` leg is set, the `-native`
  and `-native-runner` emit paths too. It needs a Martian build
  (`MARTIAN_BIN`, default `~/workdir/martian/bin`) and is skipped by
  `make test-e2e`.

A fixture in both `goldenCases` and `TestMrpDiff`'s cases is the strongest: its
JSON golden cannot silently drift from mrp, and its snapshot stays fast for the
common `make test-e2e` path. Prefer adding new correctness fixtures to both.

## Regenerating goldens

Published trees: `MARTIAN_BIN=<martian>/bin make record-goldens`. It runs every
`goldenCases` fixture not in `jsonOnlyGoldens` through mrp the way
`TestMrpDiff` does and replaces each `expected/outs/` with what mrp published.
Never edit `expected/outs/` by hand; `TestGoldenTreeTables` rejects a tree the
recorder would not write. Git cannot hold an empty directory, so the recorder
fails if mrp publishes one.

Outputs JSON: run `mrp <fixture>/pipeline.mro mrp --jobmode=local --disable-ui
--nopreflight` with `MROPATH` set to the fixture dir, and copy the
path-normalized `<pipestance>/<PIPELINE>/fork0/_outs` into `expected/`. Add the
fixture to `TestMrpDiff`'s cases so the copy is machine-checked.
