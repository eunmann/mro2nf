# Golden fixtures

Each directory is one pipeline fixture: `pipeline.mro`, its `stages/<stage>/`
code, and an `expected/` tree holding the **golden** the transpiled Nextflow run
must reproduce.

## Provenance — where a golden comes from

A golden is a real `mrp` output, not a hand-authored guess. It is produced by
running the fixture through Martian's `mrp` and keeping the path-normalized
pipeline outputs (`expected/outs.json` and any published file leaves). So the
golden encodes *mrp's* behavior for that pipeline shape.

Two suites consume it, at different strengths:

- **`TestGolden`** transpiles the fixture and asserts the Nextflow run's
  `results/pipeline_outs.json` (and published leaves) equals the committed
  golden. This is a fast, hermetic snapshot — it needs no `mrp` — but on its own
  it only proves the transpiler is self-consistent with a *frozen* snapshot.
- **`TestMrpDiff`** re-derives the truth **live**: it runs `mrp` and the
  transpiled Nextflow project against the same invocation and diffs their
  outputs (byte-for-byte file leaves + normalized `_outs`). Fixtures listed in
  its `cases` therefore machine-check the golden's mrp provenance on every run,
  and — where a `native` / `runner` leg is set — check the `-native` and
  `-native-runner` emit paths against live `mrp` too, not just the snapshot.

A fixture that appears in **both** is the strongest: its golden cannot silently
drift from `mrp` (the differential would fail), and its snapshot stays fast for
the common `make test-e2e` path (which skips `TestMrpDiff`). Prefer adding new
correctness fixtures to both, so a unit-tested code path also gains live
byte-parity coverage.

## Regenerating a golden

Run `mrp <fixture>/pipeline.mro mrp --jobmode=local --disable-ui --nopreflight`
with `MROPATH` set to the fixture dir, then copy the normalized pipeline outputs
into `expected/`. `TestMrpDiff` (with `MARTIAN_BIN` pointing at a Martian build,
default `~/workdir/martian/bin`) will confirm the regenerated golden matches both
`mrp` and the transpiled run.
