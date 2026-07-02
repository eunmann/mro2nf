# Data-movement benchmarks

`make bench` measures how much data the generated Nextflow moves, so
"performant / not needlessly copying" is a number rather than a claim. It is the
baseline and regression gate for the data-plane epic (idiomatic, emit-once
Nextflow — issues #12–#17).

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
BENCH_PROFILE=awsbatch BENCH_WORKDIR=s3://bucket/work make bench
```

The default is the local executor so CI needs no cloud credentials. On a shared
filesystem (local, HealthOmics) a staging is a cheap hard-link; on S3 it is a
real object transfer. The `refs` metric below counts stagings either way, so the
local number is the portable stand-in for the S3-object metric.

## Benchmarks

Both live under `bench/` and exercise the two costly patterns the epic targets:

- **`chain`** — one file (`MAKEBIG`) carried through a deep chain of pass-through
  stages (`PASSFILE` ×4). The pre-epic bundle data plane re-materialized the file
  into a new bundle at every bind/stage hop, transferring it O(chain depth)
  times; emit-once routing (#14) and the BIND fold (#16) cut that (see the
  baseline below).
- **`split`** — a split stage whose large stage-level file arg (`payload`) is
  broadcast to every chunk via `chunks.combine(args)`, even though `main` never
  reads it: O(N) redundant staging over an N-way split. Remaining headroom for
  consumer-aware split staging (#15).

## Metrics

Reported per benchmark (`test/e2e/bench_metrics.py`), combining Nextflow's own
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

## The gate

`make bench` fails if any benchmark's `refs` exceeds the committed baseline —
i.e. a change reintroduced per-hop re-transfer. Equal is fine; the target is
monotone improvement toward multiplier 1. After a data-plane change legitimately
lowers the numbers, re-record with `BENCH_UPDATE=1 make bench` and commit the new
`bench/baseline.json` in the same change, so the gate ratchets downward.

The S3-object-level byte count (the true network metric) is out of Nextflow's
trace; on a backend with S3 request metrics or a wrapping proxy, collect it there
and compare against these shapes.

## Baseline (local executor)

`bench/baseline.json` is the source of truth for the current numbers (recorded
after the data-plane epic landed: de-bundle #13, emit-once routing #14, BIND
folding #16, funnel-free publish #12). For scale: the pre-epic bundle-copy
design measured a ×11.0 transfer multiplier on `chain` and ×21.0 on `split`;
the epic nearly halved chain's, while split's barely moved — its residual (the
payload staged to every chunk) is the acknowledged floor pending
consumer-aware split staging (#15). Further drops re-record via
`BENCH_UPDATE=1 make bench` so the gate ratchets downward.
