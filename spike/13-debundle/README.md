# Spike: de-bundle the data plane (#13)

This spike answers the one real unknown issue #13 flags before committing to the
`.nf` shape: **can a stage's outputs be carried as per-output-param `path`
channels + a small typed sidecar, staged by Nextflow (not copied by `mre`), for
the nastiest Martian shapes — on both a shared FS and an object store?**

Run it:

```sh
bash test/e2e/spike_debundle.sh
```

It runs `main.nf` on the local executor (symlink work dir) and again with
`process.scratch + stageInMode/stageOutMode=copy` (the S3 proxy — every staging
is a physical copy, as on S3/HealthOmics-copy), asserting all four shapes stage
correctly and that no bundle `f/` byte-copy is produced.

## The model (Option E)

Each output param becomes a channel item `tuple(param, sidecar, leaves)`:

- **`sidecar`** — `<param>.sidecar.json`, the typed value tree with each file leaf
  replaced by a marker `@spike:file:<param>.LNNNN` (`@mre:file:` in the real
  shim). Nesting lives here.
- **`leaves`** — the flat, ordinal-named leaf files (`<param>.L0000`, `L0001`, …),
  captured by `path("${param}.L*")` as individual `path` items. The producer
  writes each real file **once**; there is no `f/` copy for transport.

Because nesting lives only in the sidecar, every shape — `map<file[]>`,
struct-of-file-array, and arbitrary deep combinations — collapses to "typed tree
+ flat ordinal leaf set". The leaf order is the deterministic canonical walk
(arrays by index, maps by sorted key, structs in field order), so `-resume`
caching stays stable.

## What it proves (all green, local + S3 proxy)

1. **`map<file[]>`** (MapDim=2) — dynamic keys, inner arrays. `OK[map_file_array]`
2. **struct-of-file-array** — nested leaves in a struct. `OK[struct_file_array]`
3. **split shared + per-chunk** — each chunk stages its own shard leaves (`c/`)
   plus the ONE shared stage-level leaf set (`s/`); the shared file is a single
   object Nextflow stages into each chunk, never re-materialized per chunk.
   `OK[chunk0:shared]`, `OK[chunk0:shard]`, `OK[chunk1:*]`
4. **zero-chunk join** — `Channel.empty().collect().ifEmpty([])` still runs JOIN
   once. `OK[zerojoin]`

Plus **multi-input non-collision** (`OK[multi]`) and **zero-copy** (each producer
leaf materialized once; no `f/` dir).

## The one fiddly mechanic — multi-input namespacing

Two inputs can carry leaf sets with the *same* ordinal names (e.g. two forks of a
`map<file[]>` being merged, both param `p`, both with `p.L0000`). Staging each
input under its **own directory** (`a/`, `b/`) is what makes them non-colliding;
the sidecar's markers resolve within that input's directory. In `main.nf`:

```nextflow
input:
    tuple val(p0), path('a/sidecar.json'), path('a/*')
    tuple val(p1), path('b/sidecar.json'), path('b/*')
```

This replaces the guarantee today's per-input bundle directories give for free.

## Implications for the emitter rework

- **Per output param, emit `tuple(sidecar, leaves)`** with `path("${p}.L*")`;
  `mre` writes the sidecar + ordinal leaves and does zero byte-copy.
- **Every consumer stages each input under a private dir** (`in_<id>/…`) so leaf
  sets never collide — this is the general replacement for per-input bundle dirs.
- **`mre` phase read** resolves `@mre:file:` markers against the per-input staged
  dir instead of a single bundle root; `MarkFiles`↔`resolveMarkers` stays a
  matched pair, now over ordinal leaves rather than an `f/` tree.
- **Basename is recovered at publish** from the manifest (`OutFilename`), so
  ordinal leaf names in transport are fine.
- Splits and zero-chunk joins need no special leaf handling — they reuse the same
  per-input staging; only the chunk/shared channel wiring differs.

The `bin/emit.py` / `bin/check.py` helpers stand in for `mre`'s sidecar
read/write; the spike deliberately exercises only the Nextflow staging
declarations, which is where the unknown lived.
