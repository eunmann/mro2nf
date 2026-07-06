# CellRanger `count` end-to-end: mrp vs generated Nextflow (local)

A point-in-time report of running 10x Genomics **CellRanger 10.1.0** `cellranger
testrun` (the `SC_RNA_COUNTER_CS` / `count` pipeline on the bundled ~1,084-cell
tiny dataset) two ways on one machine and comparing them:

1. **Martian** — `cellranger testrun` (real `mrp`), the golden baseline.
2. **Generated Nextflow** — `mro2nf` transpiles the same `.mro`, then Nextflow
   runs it, each stage executed by the `mre` shim against the **original**
   CellRanger stage code (Python stages + the `cr_lib`/`cr_vdj`/`cr_aggr`/`cr_ana`
   Rust binaries via `mrjob`).

CellRanger is **not** committed to this repo. The run reads a locally-downloaded
bundle; the harness is skip-if-absent.

## Headline result

The full `count` pipeline runs to completion under the generated Nextflow and its
published outputs are **byte-identical to `mrp`** — same 1,084 cells, 461,083
reads, and every mapping metric in `metrics_summary.csv` — across the default
emit and all four opt-in optimizer variations.

Getting there surfaced **five real transpiler/runtime bugs**, all now fixed with
regression tests (this branch). A real production pipeline exercises paths the
synthetic fixtures never did.

## Bugs found and fixed

| # | Bug | Fix |
|---|---|---|
| 1 | A comp/exec stage whose `src` is a bare command resolved on PATH (`cr_lib martian <subcmd>`) was baked as a nonexistent file path `mro/rna/cr_lib`, so `mrjob` failed `fork/exec … no such file`. | Keep a bare command unqualified through `resolveSrcPath` + `stageCodePaths` (Martian's `FindPath` does the same) so PATH resolves it. |
| 1b | Launching the bare command left the process's `argv[0]` bare; `cr_lib` resolves its own executable from `argv[0]` (`martian::utils::current_executable`) and panicked. | `mre` `LookPath`-resolves a bare comp/exec stagecode to an absolute path before exec. |
| 2 | The transport leaf-flatten renamed every file leaf to `f/L%04d`, dropping the extension. Martian typed-file readers reconstruct a path from the filetype (`bincode` → `with_extension`), so they looked for `f/L0002.bincode` beside a bare `f/L0002` and failed. `FILTER_BARCODES` join. | Preserve the source extension in the leaf name (`f/L0002.bincode`); applied in the Go shim and the native-runner Python port. |
| 4 | A stage RESERVES production-sized memory (the cloupe join asks for 30 GB). On a smaller machine Nextflow's local executor **parks** a task whose request exceeds the machine — forever — deadlocking a run that `mrp` completes fine (`mrp` clamps a job to its `--localmem` pool and runs it). | Emit `process.resourceLimits` in the local profile, clamping each task's request to the detected host resources. The generated pipeline now runs wherever `mrp` runs. |
| 5 | The `-native-runner` direct-Python runner leaked a numpy `np.float64(…)` repr into bundle JSON (numpy ≥ 2.0 repr is not valid JSON), which Nextflow can't parse. `SUBSAMPLE_READS`. | Coerce numpy scalars/arrays via `.tolist()` before encoding, matching `martian_shell.py`. |

## Baselines and the lever matrix

One machine: 32 cores / 31 GB. Martian: `--localcores=8 --localmem=16`. Nextflow
variations: transpiled with the named flags and run with an aggressive local
scheduling config (large `executor.memory` pool + tight polling — safe only
because the test data is tiny; the per-task `resourceLimits` clamp still applies).

| Run | Wall | Total tasks | Stage | Plumbing | Peak concurrency | Outputs |
|---|---|---|---|---|---|---|
| **Martian (`mrp`)** | **68 s** | in-process | — | **0** (in-core) | 22 | golden |
| Nextflow `default` | 104 s | 223 | 108 | 115 | 11 | ✅ identical |
| Nextflow `-native` | 104 s | 220 | 108 | 112 | 11 | ✅ identical |
| Nextflow `-native -native-runner` | 105 s | 220 | 108 | 112 | 11 | ✅ identical |
| Nextflow `-fold-disables -fuse-chains` | 105 s | 223 | 108 | 115 | 11 | ✅ identical |
| Nextflow *all four* | 105 s | 220 | 108 | 112 | 11 | ✅ identical |

"Plumbing" = data-plane tasks Nextflow externalizes that Martian does in-process
(`BIND`/`FORK`/`MERGE`/`DISABLE`/`PUBLISH`/`LAYOUT`/`BUILD_ENTRY_ARGS`).

## What the differences actually are

- **Faithfulness is total.** Every variation reproduces `mrp`'s outputs
  byte-for-byte. The two contracts (stage ABI, pipeline outputs) hold on a real
  pipeline, not just fixtures.

- **The wall-time gap (~68 s → ~104 s, ~1.5×) is orchestration overhead, not
  compute.** Median Nextflow task compute is ~0.02 s and the sum of all task
  wall-time is ~90 s; packed at the observed concurrency that is ~10–20 s of
  actual work. The rest is per-task startup — a `.command.run` wrapper → input
  staging → the `mre`/Python (or `mrjob`/Rust) launch, with the heavy cost being
  **CellRanger's Python-library import on every py task** — paid along the DAG's
  critical path. `mrp` runs stages as lean in-process jobs and pays almost none
  of this, and oversubscribes (22-wide on 8 cores) where Nextflow's local
  executor honors reservations more strictly (11-wide).

- **The opt-in levers cut orchestration *structure*, not local wall time.**
  `-native` drops a few plumbing tasks (`BUILD_ENTRY_ARGS` + tighter scatter);
  `-fold-disables`/`-fuse-chains` find little to prune or fuse here because
  CellRanger's disables are **runtime-data-dependent** (not entry-constant-
  foldable) and few chains are single-consumer equal-resource; `-native-runner`
  removes the `mre`→`martian_shell.py` process hop but not the dominant Python
  import. So wall time is flat across all variations. Their value is where the
  bench gate measures it — task/plumbing count, bytes staged, container launches
  — which translates to **cost and scale on a real backend (AWS Batch), not
  local seconds on tiny data**. On production-sized inputs, stage compute
  dominates and this overhead amortizes away.

- **A generated pipeline that deadlocks where `mrp` runs is a bug (fixed).** The
  cloupe-join park (bug 4) was not a wiring error — it was Nextflow strictly
  honoring a 30 GB reservation on a 31 GB machine. The `resourceLimits` clamp
  reproduces Martian's clamp-and-run so the pipeline runs wherever `mrp` does.

## Reproducing

```sh
# 1. Golden baseline
cellranger testrun --id=golden --localcores=8 --localmem=16 --disable-ui

# 2. Transpile (point -mrjob/-shell at the bundle's Martian runtime)
mro2nf -mropath $CR/mro -mre ./mre \
       -mrjob $CR/external/martian/bin/mrjob \
       -shell $CR/external/martian/adapters/python/martian_shell.py \
       -o out __<id>.mro           # add -native/-native-runner/-fold-disables/-fuse-chains to compare

# 3. Run (source the bundle env so cr_* + the bundled python3 are on PATH)
source $CR/sourceme.bash
cd out && nextflow run main.nf -profile standard   # add -c oversubscribe.config for aggressive scheduling
```

Then diff `out/results/metrics_summary.csv` against the golden
`<id>/outs/metrics_summary.csv`.
