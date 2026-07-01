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
  stages (`PASSFILE` ×4). The current bundle data plane re-materializes the file
  into a new bundle at every bind/stage hop, so it is transferred O(chain depth)
  times. Emit-once routing (#14) should drive this toward multiplier 1.
- **`split`** — a split stage whose large stage-level file arg (`payload`) is
  broadcast to every chunk via `chunks.combine(args)`, even though `main` never
  reads it: O(N) redundant staging over an N-way split. #15 should stage a
  stage-level file only into the chunks that consume it.

## Metrics

Reported per benchmark (`test/e2e/bench_metrics.py`), combining Nextflow's own
reporting with a direct probe-file measurement:

| metric       | source | meaning |
|--------------|--------|---------|
| `tasks`      | `-with-trace` rows | task executions |
| `processes`  | trace `name` (chunk tags collapsed) | distinct processes |
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

Recorded for the current bundle-copy design:

| benchmark | tasks | processes | edges | refs | multiplier |
|-----------|-------|-----------|-------|------|------------|
| chain     | 13    | 13        | 40    | 11   | 11.0       |
| split     | 22    | 7         | 28    | 21   | 21.0       |

Both should fall sharply as de-bundling (#13), emit-once routing (#14), split
staging (#15), BIND folding (#16), and in-place publish (#12) land.
