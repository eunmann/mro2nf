# Data-movement and overhead benchmarks

`make bench` measures how much data the generated Nextflow moves and how much
orchestration it adds, so "performant / not needlessly copying" is a number
rather than a claim. It is the regression gate for the data-plane epic
(idiomatic, emit-once Nextflow — issues #12–#17) and mechanizes the CLAUDE.md
overhead rule (#119): orchestration cost must not scale with fork width or
chunk count.

## Running

```sh
make bench                    # run and gate against bench/baseline.json
BENCH_UPDATE=1 make bench      # record the current run as the new baseline
```

The harness skips cleanly when `nextflow`, `java`, or `python3` is missing, so it
is a no-op in an environment without them.

### Scale tier

```sh
make test-scale                                  # every shape at its default width
make test-scale SCALE_RUN=^TestScaleForks SCALE_WIDTH=1000
```

`make bench` runs at widths small enough for every PR. The nightly workflow
runs the scale tier (#352) weekly, one shape per job, and `TestScaleForks` at
`SCALE_WIDTH=1000` daily. Each `TestScale<Shape>`
generates an invocation of a bench fixture at a narrow width and at a wide one,
and runs both in default and `-native` mode:

| test | fixture | narrow | wide (default) | tasks per mode, wide |
|------|---------|--------|----------------|----------------------|
| `TestScaleForks` | `forks` | 4 forks | 10,000 forks | 10,005 |
| `TestScaleSplit` | `split` | 4 chunks | 10,000 chunks | 10,004 |
| `TestScaleMapSplit` | `mapsplit` (the Cell Ranger shape) | 4 samples | 2,000 samples | 10,004 |
| `TestScaleMapNested` | `mapnested` | 2 × 2 | 100 × 100 | 10,204 |
| `TestScaleMapDisabled` | `mapdisabled` | 4 forks | 2,500 forks | 10,004 |

`SCALE_WIDTH` replaces the wide width of every selected shape. The default
widths keep each job near 52 minutes: the nightly runner ran 2,020 tasks (the
1,000-fork shape in both modes at once) in 315 s, and a local 4,000-fork run
submitted tasks at a steady rate. The selected shapes run one after another,
so the default `SCALE_TIMEOUT` of 500 minutes gives each of the five 100; the
nightly runs one shape per job with `SCALE_TIMEOUT=100m`.

Each run must complete and gather every fork's or chunk's output. On top of the
`bench_metrics.py` metrics below, the tier reads a staging probe of every task
work dir: the inputs `nxf_stage` links in (bytes staged, what an object store
downloads), the files each task wrote (bytes uploaded, every hard link
counted), and the copies of the probe file among them. It wraps `mre` in a
script that logs each task's command line, and reads the driver JVM's GC log.
`test/scale` holds every mode's narrow and wide runs to these gates:

- **plumbing tasks** identical at both widths;
- **stage tasks** equal to `mrp`'s jobs at both widths;
- **bytes staged per stage task** — per stage process, the most bytes staged
  into one of its tasks — within 64 bytes of the narrow width's. A split's join,
  which reads every chunk's output, is exempt in the `split` shape, whose width
  is the chunk count;
- **probe copies per producer** identical at both widths, so no task copies a
  broadcast file into every fork;
- **longest command-line argument** within 64 bytes of the narrow width's
  (Linux caps one argument at 128 KiB, #323);
- **driver heap** after a GC under 768 MiB at the wide width (the JVM may grow
  to 2 GiB, so an overrun fails this gate rather than the run).

A gate an open issue still fails is a known failure: `scaleKnown` in
`test/e2e/scale_test.go` keys it to the issue, and the tier reports it without
failing. A known failure whose gate passes fails the tier as "stale known
failure, remove it", the rule `test/e2e/divergences.json` applies to its
issue-tracked entries; so does one that matches no gate. The known failures at
the default widths are #255 (per-fork keyed plumbing in every map over a
sub-pipeline), #285 (a stage task stages the whole pipeline-args bundle), #325
(the default FORK copies the broadcast payload into every fork) and #326 (an
element-scatter instance stages the whole collection it scatters). The byte
gates grow linearly with the width, so at a small `SCALE_WIDTH` one can fall
within its slack and report its known failure stale.

Each job writes `<shape>.jsonl` (the four metrics lines) and `<shape>.txt` (a
summary of each run and the verdicts) to `SCALE_ARTIFACTS`
(`artifacts/scale`), uploads them as the `scale-<Shape>` artifact and adds the
summary to the job's step summary.

### Other backends (portability)

The whole point of the epic is that the *same* generated project runs on any
executor — only the config profile differs. The harness takes the project as-is
and lets you point it at another backend:

```sh
BENCH_PROFILE=awsbatch BENCH_WORKDIR=/mnt/shared/work make bench
```

The default is the local executor so CI needs no cloud credentials. On a shared
filesystem (local, HealthOmics) a staging is a cheap hard-link; on S3 it is a
real object transfer. The `refs` metric below counts stagings either way, so the
local number is the portable stand-in for the S3-object metric.

**`BENCH_WORKDIR` must be a local path.** The `refs` gate is a local scan of the
work dir (`bench_metrics.count_refs`); over an object store (`s3://…`) that scan
cannot walk the tree, so `refs` reads 0 and the gate would pass vacuously —
masking a real S3-transfer regression. The harness therefore rejects a non-local
`BENCH_WORKDIR` (any `scheme://` other than `file`) rather than report a
meaningless pass. Lanes run in parallel, so a shared `BENCH_WORKDIR` gets a
per-lane subdirectory. Collecting the true S3-object metric needs S3 request
logs or a wrapping proxy and is out of scope here (see below).

## Fixtures and lanes

Six fixtures live under `bench/`, exercising the costly patterns the epic and
the overhead rule target:

- **`chain`** — one file (`MAKEBIG`) carried through a deep chain of pass-through
  stages (`PASSFILE` ×4). The pre-epic bundle data plane re-materialized the file
  into a new bundle at every bind/stage hop, transferring it O(chain depth)
  times; emit-once routing (#14) and the BIND fold (#16) cut that.
- **`forks`** — a map call fanned out over an N-element array, with an upstream
  file broadcast to every fork instance. Run at two widths
  (`pipeline_w4.mro`, `pipeline_w16.mro`): the plumbing task count must be
  identical at both, so any per-fork bookkeeping task a change reintroduces
  fails the gate. The `-native` lanes of this fixture are what prove the
  channel-native O(1) element scatter (#99) stays O(1).
- **`split`** — a split stage whose large stage-level file arg (`payload`) is
  broadcast to every chunk via `chunks.combine(args)`, even though `main` never
  reads it: O(N) redundant staging over an N-way split (headroom for
  consumer-aware split staging, #15). Run at two chunk counts
  (`pipeline_c4.mro`, `pipeline_c16.mro`) with the same width-flat plumbing
  requirement.
- **`mapsplit`** — the Cell Ranger per-sample shape: a map call over a
  sub-pipeline whose body is a split stage writing one file per sample
  (`pipeline_w4.mro`, `pipeline_w8.mro` samples).
- **`mapdisabled`** — a map call over a sub-pipeline whose body holds a call
  disabled in every fork (`pipeline_w4.mro`, `pipeline_w8.mro`).
- **`mapnested`** — a nested map: samples × a broadcast lane array, W×W leaves
  (`pipeline_w2.mro`, `pipeline_w4.mro`). testdata's `map_pipe_nested` splits
  the inner map over each outer element instead, which mrp rejects.

The three map-over-sub-pipeline fixtures still emit per-fork keyed
BIND/MERGE/DISABLE tasks (#255), so their lanes carry `known_scaling: 255`
(see the scaling gate below).

Every fixture invocation runs as two lanes: transpiled with default (bundle)
orchestration, and again with `-native` under a distinct `_native` baseline key,
so the flagship overhead-reduction path is gated with the same teeth as the
default path.

## Metrics

Reported per lane (`test/e2e/bench_metrics.py`), combining Nextflow's own
reporting with a direct probe-file measurement:

| metric       | source | meaning |
|--------------|--------|---------|
| `tasks`      | `-with-trace` rows | task executions |
| `plumbing_tasks` / `stage_tasks` | trace, split by process name | stage tasks are those of a stage-phase process — the names the emitter gives a task that runs a stage's own code (`emit.PlainStageSuffixes`/`FusedCallSuffixes`/`ScatterCallSuffixes` over the lane's lowered program); every other task is plumbing |
| `mrp_jobs`   | lane table, checked against real mrp | jobs mrp runs for the same invocation (`TestMrpDiffBenchJobs` counts its `_jobinfo` files) |
| `processes`  | trace `name` (chunk tags collapsed) | distinct processes |
| `plumbing_processes` / `stage_processes` / `plumbing` | same split | distinct plumbing vs. stage processes, and the plumbing names seen |
| `edges`      | `-with-dag` mermaid | static process wiring (BIND nodes show here) |
| `wchar`/`rchar` | trace | aggregate characters written/read (copy-volume proxy) |
| `refs`       | work-dir scan | task dirs into which the probe file is staged/materialized |
| `multiplier` | `refs / producers` | **per-file transfer multiplier (ideal = 1)** |
| `staged_bytes` / `stage_staged_bytes` / `stage_staged_bytes_max` | `nxf_stage` in each `.command.run` | bytes of the inputs staged into all tasks / stage tasks / the largest task of each stage process |
| `uploaded_bytes` / `plumbing_uploaded_bytes` | task work dirs | bytes of the files all tasks / plumbing tasks wrote, every hard link counted |
| `probe_writes` | task work dirs | files that are a copy of the probe file (ideal = `producers`) |
| `max_argv_bytes` / `max_arg_bytes` | the scale tier's `mre` wrapper | the longest `mre` command line, and its longest argument |
| `driver_heap_bytes` / `wall_seconds` | the scale tier's GC log and clock | the driver heap after a GC, and the run's wall time |

`refs` follows symlinked input dirs (`grep -R`), so a chunk that stages a shared
bundle through a symlinked input is counted — exactly the per-task transfer S3
would incur. Each benchmark's probe file carries a recognizable marker string so
every on-disk staging/materialization is locatable.

## The gates

`make bench` (`test/e2e/bench_report.py`) fails on any of three independent
checks:

- **Baseline gate** — per lane, `refs`, `plumbing_tasks`, and `tasks` must not
  exceed the committed baseline. A rise is respectively a reintroduced per-hop
  re-transfer, reintroduced data-plane bookkeeping, or extra task executions.
  Equal is fine; the target is monotone improvement. After a change legitimately
  lowers the numbers, re-record with `BENCH_UPDATE=1 make bench` and commit the
  new `bench/baseline.json` in the same change, so the gate ratchets downward.
- **Scaling gate** — lanes that run one fixture at two widths must report an
  *identical* plumbing task count at every width: constant orchestration
  overhead is acceptable, overhead scaling with fork width or chunk count is a
  bug. This gate compares the current run against itself (not the baseline), so
  it holds even under `BENCH_UPDATE=1` — the harness refuses to record a
  baseline that violates it. A lane whose `knownScaling` (in
  `test/e2e/bench_test.go`, recorded as `known_scaling` in the baseline) names
  an open issue is exempt while its plumbing still differs by width: the
  baseline gate holds each width at its recorded count, and `BENCH_UPDATE=1`
  refuses to record a higher one. Once the group is width-flat the gate fails
  until the marker is dropped, so fixing the issue turns the lane into an
  ordinary flat-gated one.
- **mrp-jobs gate** — every lane's `stage_tasks` must equal `mrp_jobs`: one
  stage task per Martian job, so stage work is neither duplicated nor hidden
  in plumbing. `TestMrpDiffBenchJobs` (in `make test-e2e` / `make
  test-mrp-diff`, which have mrp) keeps each lane's `mrp_jobs` true to real mrp.
- **outputs** — the gates above count tasks and bytes, not results:
  `TestMrpDiff/bench/<fixture>` compares each fixture's narrower-width
  invocation, in both modes, with real mrp like any testdata fixture.

The intrinsic per-width growth (the stage's own fork/chunk tasks, and stagings
of a file every fork genuinely consumes) lives in the per-width baseline keys,
where the baseline gate still catches any increase at a given width.

The S3-object-level byte count (the true network metric) is out of Nextflow's
trace; on a backend with S3 request metrics or a wrapping proxy, collect it there
and compare against these shapes.

## Baseline (local executor)

`bench/baseline.json` is the machine source of truth for the current numbers —
one entry per lane (`chain`, `forks_w4`, `split_c16`, `mapsplit_w8`, …, plus
their `_native` counterparts). For scale: the pre-epic bundle-copy design measured a ×11.0
transfer multiplier on `chain` and ×21.0 on `split`; the epic nearly halved
chain's, while split's residual (the payload staged to every chunk) is the
acknowledged floor pending consumer-aware split staging (#15). The `-native`
lanes carry less plumbing than their default counterparts — the point of #76/#99
— and the gate keeps it that way. Further drops re-record via
`BENCH_UPDATE=1 make bench` so the gate ratchets downward.
