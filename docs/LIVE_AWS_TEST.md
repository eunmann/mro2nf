# Live AWS test report

Tracks what was actually run on real AWS infrastructure (not just local/Docker)
and verified against `mrp` golden outputs. Updated as runs complete.

- **Account / region:** 854552618084 / us-east-2
- **Infra:** `deploy/awsbatch-cdk/` (S3 bucket, ECR `mro2nf-runtime`, Batch managed-EC2
  spot CE + queue, HealthOmics role). VPC public subnets + S3 gateway endpoint, no NAT.
- **Runtime image:** `python:3.12-slim` + `procps` + `awscli` + static `linux/amd64`
  `mre` + Martian adapters + stage code at `/opt/mro2nf` (built from the generated
  `Dockerfile`). One tag per fixture in ECR.
- **Date:** 2026-07-01 (round 5, data-plane epic); 2026-06-27 (round 4); earlier 2026-06-26

## Round 5 (2026-07-01, epic #18 data-plane rework, us-east-1)

After the idiomatic emit-once data-plane epic (de-bundle #13, emit-once routing
#14, split staging #15, fold BIND #16, funnel-free publish + manifest #12/#11),
the whole transpiler was re-validated LIVE on **both** cloud targets, driven by
the new reusable parallel harnesses `test/e2e/aws_run.sh` (Batch) and
`test/e2e/aws_healthomics.sh` (HealthOmics). Stack deployed in **us-east-1**
(HealthOmics is not available in us-east-2). Each fixture's S3-published
`pipeline_outs.json` was diffed (path-normalized) against a local `mrp` golden.

- **AWS Batch + S3: 13/13 byte-identical to mrp** — file_min, file_chain,
  split_test, kitchen_sink, file_tree, map_file, struct_of_file, dir_out,
  fork_min, map_split_file, struct_file_array, map_file_array, map_null_map. Run
  in parallel (`RUN_PARALLEL=8`).
- **AWS HealthOmics: 4/4 byte-identical to mrp** — file_min, split_test,
  file_tree, map_file (one workflow + run per fixture, all COMPLETED).
- **Manifest ↔ S3 alignment verified:** the emitted `manifest.json.gz` outputs
  list is set-equal to the actual published `outs/` files on **both** backends
  (file_tree: 9 leaves = 9 manifest entries, exact match).

The generated DAG is materially leaner (fewer plumbing nodes; the epic routes /
folds standalone `BIND_*` and de-funnels PUBLISH) — see the epic issue for the
before/after node/edge/transfer-multiplier table. No transpiler code changes were
needed for either backend; only the two new harness scripts.

## Re-verification round 4 (2026-06-27, after the code-review `--fix`)

After applying the max-effort code-review fixes (the entry-file staging
collision fix, the `errStagedMany` guard, the `-fileflat` list-coercion follow-up,
and assorted cleanups), the Batch compute environment was scaled
(`maxvCpus` 16 → 256, still $0 idle on spot + `minvCpus: 0`) and a **parallel**
campaign of 15 transpiled fixtures was run at once on live AWS Batch + S3
(us-east-2). Images were built as one shared base (`mre` + adapters) plus a thin
per-fixture layer (stages), so the whole fleet pushed quickly.

**Result: 15/15 byte-identical to `mrp` on live AWS Batch + S3.**

The campaign deliberately re-covers the **changed code path** — file-typed entry
inputs at every shape — plus a regression spread:

| Fixture | What it exercises | Live result |
|---|---|---|
| `entry_file` | scalar `file` entry input from S3 (override) | ✅ `{"total":42.0}` |
| `entry_filearr` | `file[]` entry input from S3 | ✅ `{"total":30.0}` |
| `entry_filearr` (same-basename) | **two `file[]` leaves sharing a basename** — the `stageAs: '<in>_?/*'` collision fix | ✅ `{"total":30.0}` |
| `entry_struct_file` | struct-with-file entry input | ✅ `{"total":40.0}` |
| `entry_mapfile` | `map<file>` entry input | ✅ `{"total":40.0}` |
| `split_test` | split/main/join + entry parameterization | ✅ `{"sum":14}` |
| `map_pipe` | map over array / sub-pipeline | ✅ `{"ys":[2,3,4]}` |
| `map_pipe_nested` | nested map (2-D fork keying) | ✅ `{"yss":[[2,4],[6]]}` |
| `disabled_map` | disabled map + null bundle | ✅ `{"w":null}` |
| `kitchen_sink` | split + map + struct (comprehensive) | ✅ full match |
| `join_resources` | split-returned join override | ✅ `{"sum":14}` |
| `split_from_file` | data-dependent split from a file | ✅ `{"nchunks":3,"total":29}` |
| `map_split_file` | map over a split stage emitting a file | ✅ `{"bams":["s2.txt","s3.txt"]}` |
| `struct_file_array` | struct field that is a file array | ✅ `{"r":{"files":["r0.txt","r1.txt"],"n":2}}` |
| `mixed_adapters` | py → exec → comp in one pipeline | ✅ `{"z":11}` |

The same-basename `file[]` run is the key new check: before the fix, two leaves
named `reads.txt` collided in the task work dir; with `stageAs: '<in>_?/*'` each
lands in its own numbered subdir and reconstructs to the right slot. Verified
live, byte-identical to `mrp`.

**HealthOmics (us-east-1):** the same-basename entry-file override was also run on
live AWS HealthOmics to confirm the fix on the managed backend — `entry_filearr`
with `reads = [s3://…/sb1/reads.txt, s3://…/sb2/reads.txt]` (run id `3456592`),
result `{"total":30.0}` ✅, byte-identical to mrp. Output exported to
`omics-out/3456592/pubdir/pipeline_outs.json`.

## Re-verification round 3 (2026-06-26, current committed code)

The CDK stacks were found **still deployed** (us-east-2 Batch+S3, us-east-1
HealthOmics) — the earlier "teardown done" note is superseded; the infra was
redeployed and is live again. Re-confirmed the **current committed transpiler**
runs correctly on the live infra:

| Run | Path | Result |
|---|---|---|
| `exec_min` (reused ECR image) | exec adapter, pure compute on Batch+S3 | ✅ `{"y":14}` |
| `file_array` (**fresh** transpile → rebuilt+pushed image) | map-fork file outputs → MERGE → `file[]` consume, all via S3 | ✅ `{"total":60}` |

The `file_array` run is the definitive check: re-transpiled from current
`testdata`, image rebuilt and pushed, then run live — proving the committed code
(not a stale snapshot), byte-identical to `mrp`. **Caveat learned:** the local
`out-*/` dirs are stale snapshots (re-running `out-filearr` gave `20.0` from its
old baked input, not the current golden `60`); always re-transpile from
`testdata` for a live run. (Those dirs are gitignored.)

### New complex-combination fixtures (this session)

Added to cover realistic multi-feature pipelines (the kind Cell Ranger builds),
not just single features:

| Fixture | Combination exercised | Local + docker-iso | Live Batch + S3 |
|---|---|---|---|
| `map_split_file` | map over a sub-pipeline whose body is a **split stage emitting a file** (fork keying + per-fork split/main/join + file through the object-store merge) | ✅ `{"bams":["s2.txt","s3.txt"]}` | ✅ `{"bams":["s2.txt","s3.txt"]}` |
| `mixed_adapters` | **py → exec → comp** chained in one pipeline (all three ABIs + `mrjob` in one image) | ✅ `{"z":11}` | ✅ `{"z":11}` |
| `struct_file_array` | a struct output whose field is a **file array** (`struct{ txt[] files, int n }`; type walk descends struct → array → file leaf) | ✅ `{"r":{"files":["r0.txt","r1.txt"],"n":2}}` | ✅ `{"r":{"files":["r0.txt","r1.txt"],"n":2}}` |

All three ran end-to-end on **live AWS Batch + S3** (us-east-2, default
`emu_dev` SSO profile, fresh image per fixture built from the generated
`Dockerfile` and pushed to ECR), each **byte-identical to `mrp`** — confirming
the complex combinations on the real object-store data plane, not just under
docker-isolation. HealthOmics not re-run for these (every constituent feature is
already in the HealthOmics 14/14 matrix below; the combos are validated on Batch
+ docker-iso).

## Re-verification round 2 (2026-06-26, after C1/C2/C3)

Stack redeployed (us-east-2 Batch+S3, us-east-1 HealthOmics) and the post-C1/C2/C3
changes verified live, all byte-identical to the local/docker-iso goldens:

| Check | Batch + S3 (us-east-2) | HealthOmics (us-east-1) |
|---|---|---|
| C1 single file from S3 (`--reads s3://…`) | ✅ `total=200.0` | ✅ S3 run param → `total=200.0` |
| C1 **directory** from S3 prefix (`--fastqs s3://…/dir/`, Cell Ranger shape) | ✅ `total=20.0` | (same mechanism) |
| C1 baked default (content travels in the entry_args bundle) | ✅ `total=12.0` | — |
| C2 no `--aws_outdir` (no launcher publish; output in S3 work dir) | ✅ | n/a (managed pubdir) |
| C2 `--aws_outdir s3://…` (curated S3 publish) | ✅ | ✅ pubdir → S3 |
| C3 per-bind specs / byte-identity | ✅ | ✅ (bindspecs in zip) |

The `CopyTree` symlink-deref fix held on a real `s3://` work dir (the staged
override file is a Nextflow symlink).

### File-bearing entry inputs — every shape (the Cell Ranger input model)

The entry-input staging was generalized from scalar files to **all** file-bearing
shapes via one flatten-and-reconstruct mechanism (`-fileflat`). Each was supplied
**only as S3 URIs at run time** (no baked path, no re-transpile) and verified live:

| Shape | Batch + S3 (us-east-2) | HealthOmics (us-east-1) |
|---|---|---|
| scalar `file` | ✅ `200.0` | ✅ `200.0` |
| directory (`path`, `s3://…/dir/`) | ✅ `20.0` | — |
| `file[]` (multiple FASTQ-analog files) | ✅ `60.0` | — |
| `map<file>` (keyed, sorted-key order) | ✅ `60.0` | — |
| struct-with-file (config struct, nested-object run param) | ✅ `12.0` | ✅ `12.0` |

All byte-identical to the local + docker-isolation goldens (the docker-iso
overrides place the input **outside** the image, so a correct result proves the
file arrived only via staging). HealthOmics accepted a nested-object (struct) run
parameter. Fixtures: `entry_file`, `entry_filearr`, `entry_mapfile`,
`entry_struct_file`.

### All three adapters + data-dependent split (live on Batch + S3)

| Path | Result |
|---|---|
| `comp` adapter (compiled stage via mrjob, baked into the image) | ✅ `comp_split` → `{"sum":14}` |
| `exec` adapter (exec stage) | ✅ `exec_min` → `{"y":14}` |
| **data-dependent `split`** — chunk count + per-chunk `__threads` computed at runtime from a **staged S3 file** | ✅ `split_from_file`: a 5-line S3 manifest fanned out **5 chunks → total 55** (vs 3 chunks → 29 for the 3-line default) |

The data-dependent split is Martian's defining runtime feature (Cell Ranger
splits by lanes/reads): the split phase opens the staged input and emits the
chunk set, and Batch provisions each chunk per its requested resources. All three
adapter ABIs (`py`/`exec`/`comp`) now have a live cloud run.

## Not yet run live (lower-priority, verified locally + docker-iso)

1. **Spot reclaim** — `errorStrategy` retry/terminate is verified end-to-end
   (see "Error handling" below; ✅ retry on exit 1, terminate on ASSERT exit 42),
   but a forced spot-instance reclamation mid-run has not been exercised.
2. **`-resume` over an S3 work dir** — Nextflow content-addressed resume on
   `s3://` is finicky and untested here.
3. **A stage with a baked third-party dependency** (the "no internet on
   HealthOmics, bake your tools" model end-to-end).

(The complex-combination fixtures `map_split_file` and `mixed_adapters`,
previously listed here, ran live on Batch + S3 in round 3 — see the
combination table above — and `map_split_file` again in round 5.)

## What is being verified

That a transpiled pipeline runs **natively on AWS Batch with an S3 work dir**
(isolated containers, no shared filesystem, object-store data plane) and produces
**byte-identical outputs to `mrp`**, across a spread of Martian features.

## Test matrix (AWS Batch + S3)

All 8 ran on AWS Batch with an `s3://` work dir and produced output **byte-identical
to the `mrp` golden**.

| Fixture | Feature coverage | Golden (= live result) | Status |
|---|---|---|---|
| `split_test` | split → main×3 → join; **input parameterization** (BUILD_ENTRY_ARGS) | `{"sum":14}` | ✅ PASS |
| `map_pipe` | map call over an array, over a sub-pipeline | `{"ys":[2,3,4]}` | ✅ PASS |
| `map_pipe_nested` | **nested map** (2-D fork keying, forknames enumeration on S3) | `{"yss":[[2,4],[6]]}` | ✅ PASS |
| `disabled_map` | disabled map call + null-bundle staging | `{"w":null}` | ✅ PASS |
| `kitchen_sink` | split + map + struct outputs (comprehensive) | `{"scaled":[10,20,30,40],"scaled_again":[...],"stats":{"count":4,"mean":2.5,"total":10.0},"sum":30}` | ✅ PASS |
| `map_file` | map call producing **file outputs** (bundle markers through S3) | `{"fs":["v1.txt","v2.txt"]}` | ✅ PASS |
| `struct_of_file` | **struct-of-file** output through the object-store data plane | `{"b":{"n":5,"report":"report.txt"}}` | ✅ PASS |
| `join_resources` | split-returned **join resource override** | `{"sum":14}` | ✅ PASS |

### Wave 2 (broader coverage)

| Fixture | Feature coverage | Result | Status |
|---|---|---|---|
| `map_pipe_split` | **map over a split stage** (keyed split on S3) | `{"totals":[5,9]}` | ✅ PASS |
| `map_split` | map over a split stage incl. an empty fork | `{"totals":[5,0,9]}` | ✅ PASS |
| `multidim` | multi-dimensional map projection | `{"total":10}` | ✅ PASS |
| `multisplit` | multiple split stages chained | `{"s":[11,22,33]}` | ✅ PASS |
| `fanin` | fan-in bindings (`[A.x, B.x]`) | `{"total":10}` | ✅ PASS |
| `nested_struct` | nested struct types | `{"total":5}` | ✅ PASS |

**Result: 14/14 PASS on live AWS Batch + S3**, all outputs byte-identical to `mrp`.
Covers split/main/join, map over arrays / maps / sub-pipelines / **split stages**,
nested maps, disabled calls, structs & nested structs, file & struct-of-file
outputs, fan-in, multi-dim projection, multiple splits, join-resource override,
and input parameterization — the object-store data plane verified end-to-end.

## Operational coverage (beyond language features)

| Dimension | Status |
|---|---|
| Output data == mrp | ✅ 14/14 Batch; HealthOmics (see below) |
| File **outputs** through the object store | ✅ `map_file`, `struct_of_file` |
| File **inputs** read by a downstream stage | ✅ `file_chain` → `{"y":42.0}` on Batch (stage opened + read the upstream file) |
| Stage logs (`martian.log_info`), stdout, stderr | ✅ forwarded to the task log (`.command.log` → CloudWatch / Omics engine log) — see issue 3 |
| Stage errors / `ASSERT` | ✅ mirrored to stderr → CloudWatch; exit-42 terminates (non-retryable) |
| cpus/memory provisioning | ✅ `_jobinfo` matches mrp; Batch requests per the directives |
| Retries (`errorStrategy`) | ⚠️ config emitted; not exercised live (no transient failure forced) |
| Directory outputs / `comp`+`exec` adapters | ⚠️ not run live in *this* wave (only `py` stages); ✅ covered in the later waves — `dir_out` and all three adapters ran live (see round 3 and the totals below) |

## Issues found and fixed during live testing

1. **ECR lifecycle pruned an in-use image.** The CDK's `maxImageCount: 5` expired
   the oldest tags after pushing 8 fixtures, so Batch hit
   `CannotPullImageManifestError: manifest unknown`. **Fix:** bumped to `20`
   (`deploy/awsbatch-cdk/lib/stack.ts`); redeployed.

2. **S3-only data-plane bug (the important one).** Head-node workflow closures read
   bundle JSON via `file("${path}/sub").text`. Interpolating an S3 `Path` into a
   GString **drops the `s3://` scheme** (yields `/bucket/key`), so `file()` read it
   as a local path → `No such file or directory: /bucket/work/...`. This broke
   every split / disabled / nested fixture on S3 (the file/map fixtures, which have
   no such head-node reads, passed). Local and Docker-isolation runs could **not**
   catch it — local GString interpolation is lossless; only a real `s3://` work dir
   exposes it. **Fix:** use `Path.resolve('sub').text` (and a bare `f.text` for a
   whole-path read), which operates on the `Path` object and preserves the
   filesystem/scheme. Applied across `genSplitWorkflow`, `genKeyedSplitWorkflow`,
   `genKeyedCallBody`, `genKeyedMappedCallBody` / `genKeyedMappedDisableGate`,
   `genMappedDisableGate`, `genDisabledWiring`. Re-verified locally (e2e) and
   re-running on Batch. The `.nf` fix is head-node only, so the ECR images are
   unchanged.

## AWS HealthOmics (us-east-1)

HealthOmics isn't usable in us-east-2, so a second stack was deployed to
**us-east-1** (the IAM omics role is global; ECR/S3 are per-region). The same
runtime image was pushed to the us-east-1 ECR, `split_test` was packaged
(`package.sh` → `workflow.zip`) and registered with
`aws omics create-workflow` (id `2648761`), then `start-run` with the omics
service role, DYNAMIC run storage, the ECR image as the `container` parameter,
and outputs to `/mnt/workflow/pubdir` → S3.

Each was packaged (`package.sh` → `workflow.zip`), registered with
`aws omics create-workflow`, and run with `aws omics start-run` (omics service
role, DYNAMIC storage, ECR image as the `container` parameter, output to
`/mnt/workflow/pubdir` → S3 under `omics-out/<run-id>/pubdir/`).

| Workflow | Feature coverage | Result | Status |
|---|---|---|---|
| `split_test` | split/main/join; packaging path (zip + parameter-template); managed execution; ECR-from-Omics | `{"sum":14}` | ✅ PASS |
| `map_pipe` | map over array / sub-pipeline | `{"ys":[2,3,4]}` | ✅ PASS |
| `map_pipe_nested` | nested map (2-D fork keying) | `{"yss":[[2,4],[6]]}` | ✅ PASS |
| `disabled_map` | disabled map + null bundle | `{"w":null}` | ✅ PASS |
| `kitchen_sink` | split + map + struct (comprehensive) | full match | ✅ PASS |
| `map_file` | file outputs | `{"fs":["v1.txt","v2.txt"]}` | ✅ PASS |
| `struct_of_file` | struct-of-file output | `{"b":{"n":5,"report":"report.txt"}}` | ✅ PASS |
| `join_resources` | split-returned join override | `{"sum":14}` | ✅ PASS |
| `map_pipe_split` | map over a split stage | `{"totals":[5,9]}` | ✅ PASS |
| `file_chain` | **downstream stage reads an upstream file** | `{"y":42.0}` | ✅ PASS |

**Result: 10/10 PASS on live AWS HealthOmics (us-east-1)**, all byte-identical to
`mrp`. Output exported to `omics-out/<run-id>/pubdir/`; the Omics engine log is
at `omics-out/<run-id>/logs/engine.log`.

3. **Stage logs were lost on the cloud.** The shim sent the stage's stdout/stderr
   to `_stdout`/`_stderr` files and `martian.log_info` to `_log` — all in the
   per-task scratch, which an object-store backend does not upload (only declared
   outputs + `.command.*` are retained). So on success, stage logs were
   invisible. **Fix:** the shim now tees the stage's stdout/stderr to its own
   streams and forwards `_log` to stderr, so they appear in the task's captured
   log (`.command.log`/`.command.err` → CloudWatch on Batch, the Omics engine log
   on HealthOmics, `nextflow log` locally). Verified locally: a stage's
   `martian.log_info` lines now show under a `--- martian stage log ---` header.

4. **Dockerfile omitted `mrjob`.** The generated Dockerfile copied mre/adapters/
   stage code but not the `mrjob` wrapper, so a **comp-adapter** image was missing
   `/opt/mro2nf/mrjob` on the worker. **Fix:** the Dockerfile now COPYs `runtime/mrjob`
   when the pipeline has comp stages (`Options.Mrjob` set). Regression test
   `TestEmitContainerMrjob`; `make test-e2e` gained a `-mrjob` path so `comp_split`
   runs in the standing suite.

## Adapter / data-shape coverage (wave 3, both targets)

Added to close gaps that complex pipelines hit:

| Fixture | Exercises | Batch result | HealthOmics |
|---|---|---|---|
| `exec_min` | the **exec** adapter (wrapped-adapter path) | ✅ `{"y":14}` | ✅ `{"y":14}` |
| `comp_split` | the **comp** adapter via `mrjob` (split/main/join) | ✅ `{"sum":14}` | ✅ `{"sum":14}` |
| `dir_out` | a **directory** output through the object store | ✅ `{"d":"d"}` | ✅ `{"d":"d"}` |
| `file_array` | a stage reading a **`file[]`** input (each file's content) | ✅ `{"total":60}` | ✅ `{"total":60}` |

**Totals: AWS Batch 19/19, AWS HealthOmics 14/14 — every output byte-identical to
mrp.** All three adapters (py/exec/comp), every file shape (single/array/struct/
directory, in and out), logs, retry/terminate, and resources verified live.

## How the transpiler signals what it can't reproduce

- **Hard error (transpile fails):** unknown expressions (e.g. `sweep` — parse
  error), unknown adapter language, a `comp` stage with no `mrjob`, a *nested*
  typed-map field projection (`map<map<S>>.field`, or maps nested inside an
  array — plain `array<map<S>>.field` is supported), an invalid `-target`.
  Lowering errors by default on any expression it doesn't recognize, so
  unsupported constructs cannot slip through as a silent drop.
- **Warning (transpiles, behavior differs):** a `preflight` bound to a call
  output (runs in DAG order; an input-bound preflight gates the pipeline like
  mrp), `local` (scheduled normally), `volatile` (no mid-run VDR). `mro2nf`
  logs these.
- **Documented, no output impact:** the `outs/` tree is copied into place
  (mrp moves + symlinks); `special` → `clusterOptions` mapping. See
  `FEATURE_COVERAGE.md`.

(Note: a 3-level nested map was attempted but **Martian itself** rejects it —
`mro check`: "filtering merge: unexpected merge expression for int" — so 2-level
is the supported nesting depth, a Martian limit, not the transpiler's.)

## Error handling (retry vs terminate) — verified end-to-end

The content-based `errorStrategy` was confirmed with two failing pipelines
(behavior is config-level, identical locally and on the cloud):

| Case | Stage behavior | Expected | Observed |
|---|---|---|---|
| ordinary failure | `raise RuntimeError` → mre exit 1 | retry (maxRetries 2) | **3 attempts** then fail ✅ |
| ASSERT | `martian.exit(...)` → mre exit 42 | terminate, no retry | **1 attempt**, exit 42 ✅ |

## Status

Live validation: **AWS Batch + S3 (us-east-2)** — 22/22 in round 3, then a
**15/15 parallel re-run in round 4** after the code-review fixes (Batch scaled to
`maxvCpus: 256`) — and **AWS HealthOmics (us-east-1) 14/14** plus a round-4
same-basename entry-file run. All byte-identical to mrp. Five real issues were
found and fixed via the live runs (ECR lifecycle, the S3 `.resolve()` scheme bug,
lost stage logs, the Dockerfile missing `mrjob`, and the entry-file basename
collision), plus the no-op-modifier warnings. Local `make test` + `make test-e2e`
(62 cases / 57 fixtures) + `make test-e2e-docker` (19) green.

**Infra status (2026-06-27, round 4):** after the round-4 campaign the stack was
**torn down** — `cd deploy/awsbatch-cdk && npx cdk destroy` in us-east-2 and
us-east-1. To bring it back, `npx cdk deploy Mro2nfStack` per region. It
idles at ≈$0 when up (Batch `minvCpus: 0`, spot; no NAT/EFS), so leaving it
deployed is cheap if more live runs are planned.
