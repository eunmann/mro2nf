# Golden fixtures

Each directory is one pipeline fixture: `pipeline.mro`, its `stages/<stage>/`
code, and an `expected/` tree holding the **goldens** the transpiled Nextflow
run must reproduce. A golden has two parts:

- **The outputs JSON** (`expected/outs.json`, or the file the `goldenCases`
  entry in `test/e2e/golden_test.go` names): mrp's path-normalized pipeline
  `_outs`. It is copied in by hand from an mrp run, so its provenance is a
  convention, not a mechanism. Fixtures mrp cannot run (`mrpUnrunnable` in
  `test/e2e/coverage_test.go`, e.g. `exec_min`, `mixed_adapters`) have JSON
  goldens that no mrp run produced.
- **The published tree** (`expected/outs/`): every file mrp published under the
  pipestance's `outs/`, byte for byte; mrp keeps its outs JSON outside `outs/`
  and writes no manifest. Git cannot hold an empty directory, so the empty
  directories mrp published are listed instead in `expected/outs.emptydirs`,
  one path relative to `outs/` per line. Only `make record-goldens` writes
  either (see below). A fixture with neither published nothing under mrp.

## What the suites check

- **`TestGolden`** transpiles the fixture, runs it under Nextflow, and
  requires:
  - `results/pipeline_outs.json` to equal the outputs JSON, number tokens and
    object key order included; and
  - `results/`, minus `pipeline_outs.json` and `manifest.json.gz` at its root
    (allowlist entry `results-metadata-files`), to equal `expected/outs/`: no
    file missing, no file extra, every file's sha256 equal, no file published
    as a symlink, and the same empty directories.

  Cases of `mrpUnrunnable` fixtures, and the `*_override` cases in
  `jsonOnlyGoldens` (`test/e2e/goldentree_test.go`), which rerun a fixture
  with a launch-time `-params-file` mrp has no equivalent for, skip the tree
  check. These suites need no `mrp`, so they compare against a frozen snapshot.
- **`TestMrpDiff`** re-derives the truth **live**: it runs `mrp` and the
  transpiled Nextflow project on the same invocation and diffs the published
  trees (same comparison as above), the normalized `_outs` and every stage
  phase's `_args`/`_outs` for each fixture in its `mrpDiffCases`, so each
  outputs JSON is machine-checked against mrp.
- **`TestMrpDiffFailureParity`** runs the fixtures mrp fails on by design
  (`mrpOutcomeCases`) under both runners and requires that mro2nf fails exactly
  when mrp does, blaming the same stages with the same kind of failure (an
  ASSERT or an error). A case where the outcomes differ because of an open bug
  records that issue, and the case fails once the bug is fixed.

  Every fixture is in exactly one of `mrpDiffCases`, `mrpOutcomeCases` and
  `mrpUnrunnable` (with the reason mrp cannot be compared on it). Both suites
  need a Martian build (`MARTIAN_BIN`, default `~/workdir/martian/bin`);
  `make test-e2e` includes them and CI runs them on every PR.

## Regenerating goldens

Published trees: `MARTIAN_BIN=<martian>/bin make record-goldens`. It runs every
`goldenCases` fixture outside `mrpUnrunnable` and `jsonOnlyGoldens` through mrp
the way `TestMrpDiff` does and replaces each `expected/outs/` and
`expected/outs.emptydirs` with what mrp published. Never edit either by hand;
`TestGoldenTreeTables` rejects one the recorder would not write.

Outputs JSON: run `mrp <fixture>/pipeline.mro mrp --jobmode=local --disable-ui
--nopreflight` with `MROPATH` set to the fixture dir, and copy the
path-normalized `<pipestance>/<PIPELINE>/fork0/_outs` into `expected/`. Add the
fixture to `mrpDiffCases` so the copy is machine-checked (or, if mrp fails on it
by design, to `mrpOutcomeCases`, or to `mrpUnrunnable` with the reason mrp
cannot run it); `TestEveryFixtureMrpDiffed` requires exactly one.
