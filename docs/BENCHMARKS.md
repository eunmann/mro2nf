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

Three fixtures live under `bench/`, exercising the costly patterns the epic and
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
| `plumbing_tasks` / `stage_tasks` | trace, split by process name | tasks that add no Martian stage work (`BIND_*`/`FORK_*`/`MERGE_*`/`DISABLE_*`, `BUILD_ENTRY_ARGS`, `LAYOUT`, `PUBLISH_LEAF`) vs. real stage tasks |
| `processes`  | trace `name` (chunk tags collapsed) | distinct processes |
| `plumbing_processes` / `stage_processes` / `plumbing` | same split | distinct plumbing vs. stage processes, and the plumbing names seen |
| `edges`      | `-with-dag` mermaid | static process wiring (BIND nodes show here) |
| `wchar`/`rchar` | trace | aggregate characters written/read (copy-volume proxy) |
| `refs`       | work-dir scan | task dirs into which the probe file is staged/materialized |
| `multiplier` | `refs / producers` | **per-file transfer multiplier (ideal = 1)** |

`refs` follows symlinked input dirs (`grep -R`), so a chunk that stages a shared
bundle through a symlinked input is counted — exactly the per-task transfer S3
would incur. Each benchmark's probe file carries a recognizable marker string so
every on-disk staging/materialization is locatable.

## The gates

`make bench` (`test/e2e/bench_report.py`) fails on either of two independent
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
  baseline that violates it.

The intrinsic per-width growth (the stage's own fork/chunk tasks, and stagings
of a file every fork genuinely consumes) lives in the per-width baseline keys,
where the baseline gate still catches any increase at a given width.

The S3-object-level byte count (the true network metric) is out of Nextflow's
trace; on a backend with S3 request metrics or a wrapping proxy, collect it there
and compare against these shapes.

## Baseline (local executor)

`bench/baseline.json` is the machine source of truth for the current numbers —
one entry per lane (`chain`, `forks_w4`, `split_c16`, …, plus their `_native`
counterparts). For scale: the pre-epic bundle-copy design measured a ×11.0
transfer multiplier on `chain` and ×21.0 on `split`; the epic nearly halved
chain's, while split's residual (the payload staged to every chunk) is the
acknowledged floor pending consumer-aware split staging (#15). The `-native`
lanes carry less plumbing than their default counterparts — the point of #76/#99
— and the gate keeps it that way. Further drops re-record via
`BENCH_UPDATE=1 make bench` so the gate ratchets downward.
