# CellRanger on a public dataset (pbmc_1k_v3), end-to-end on AWS

Runs the **real Cell Ranger `count` pipeline** on a standard **public** dataset,
transpiled by mro2nf and executed on AWS HealthOmics with **every input staged
from S3 — nothing baked into the container**. This is the customer model: point
the pipeline at fastqs + a reference in S3 and get CellRanger outputs back.

Complements `docs/CELLRANGER_LOCAL.md` (local toy testrun) and
`docs/LIVE_AWS_TEST.md` (per-fixture live matrix).

- **Account / region:** 854552618084 / us-east-1 (HealthOmics is not in us-east-2)
- **Infra:** CDK stack `Mro2nfStack` — work bucket, ECR `mro2nf-runtime`,
  HealthOmics service role.
- **Date:** 2026-07-07

## The dataset

**1k PBMCs from a Healthy Donor, v3 chemistry** — 10x Genomics' canonical
tutorial/benchmark dataset, and a realistic customer run:

| | Value |
|---|---|
| Sample | `pbmc_1k_v3` (SC3Pv3 chemistry) |
| FASTQs | `pbmc_1k_v3_fastqs.tar` (~5.2 GB): R1 (barcode/UMI), R2 (cDNA), I1 (index), 2 lanes |
| Read pairs | ~66.6 M |
| Reference | `refdata-gex-GRCh38-2020-A` (~11 GB tarball, ~18 GB in S3) — full human transcriptome |

Both are free, registration-free downloads from the 10x CDN:

```
wget https://cf.10xgenomics.com/samples/cell-exp/3.0.0/pbmc_1k_v3/pbmc_1k_v3_fastqs.tar
wget https://cf.10xgenomics.com/supp/cell-exp/refdata-gex-GRCh38-2020-A.tar.gz
```

Tutorial: <https://www.10xgenomics.com/support/software/cell-ranger/latest/tutorials/cr-tutorial-ct>

## 1. Stage the inputs to S3 (no baking)

Extract both archives and upload the fastq directory and the reference directory
to the work bucket:

```
s3://<WorkBucket>/pbmc/fastqs/                     # the 6 fastq.gz files
s3://<WorkBucket>/pbmc/refdata-gex-GRCh38-2020-A/  # fasta/ genes/ pickle/ star/ reference.json
```

**Fast upload for a slow uplink (the relay pattern).** A ~20 GB upload over a
slow home connection (~30 Mbps here) is bandwidth-bound (~90 min); higher AWS-CLI
concurrency or `s5cmd` do not help. An in-region EC2 relay does 10x-CDN → EC2 →
S3 (same region) at Gbps in ~5 min. Deploy it as a **tracked CloudFormation
stack** so nothing leaks — the instance's user-data `wget`s + extracts +
`aws s3 cp`s to the bucket, writes `pbmc/_relay_done.txt`, and self-stops; poll
for the marker, then `aws cloudformation delete-stack` to tear it all down (EC2 +
IAM role + SG in one shot).

## 2. Runtime image (lean — toolkit + runtime only, no data)

`mro2nf-runtime:cr-overhead` = the CellRanger toolkit (`cr_*` binaries + bundled
Python + Martian runtime, on PATH at `/opt/cr`) plus the mro2nf runtime (`mre` +
adapters + mrjob + stage code at `/opt/mro2nf`). **No dataset baked in** — the
reference and fastqs come from S3 at run time. Build it `FROM …:cellranger` and
`COPY` the four mro2nf runtime layers over `/opt/mro2nf`, then push to ECR.

## 3. Register the workflow

Transpile the `/opt/cr`-path mro (`cr-ho-build/__tr_dry.mro`) for HealthOmics,
`package.sh`, and `aws omics create-workflow`. Transpile from *that* mro (not the
`~/Downloads` one, which has local paths and bakes 104 MB of reference bytes into
`workflow.zip`, blowing past HealthOmics' 10 MiB definition limit). The workflow
is reusable across datasets — inputs are supplied at launch.

## 4. Launch (DYNAMIC storage, all inputs as S3 URIs)

Override the baked defaults with the S3 dataset. **Directory inputs must end in a
trailing `/`** — HealthOmics `start-run` rejects a bare prefix with "S3 object
not found":

```
aws omics start-run --workflow-id <id> --role-arn <OmicsRole> \
  --name cellranger-pbmc-1k-v3 \
  --output-uri s3://<WorkBucket>/omics-out/ --storage-type DYNAMIC \
  --parameters '{
    "container": "<ecr>/mro2nf-runtime:cr-overhead",
    "sample_id": "pbmc_1k_v3",
    "reference_path": "s3://<WorkBucket>/pbmc/refdata-gex-GRCh38-2020-A/",
    "sample_def": [{
      "fastq_mode": "ILMN_BCL2FASTQ", "library_type": "Gene Expression",
      "read_path": "s3://<WorkBucket>/pbmc/fastqs/",
      "sample_indices": ["any"], "sample_names": ["pbmc_1k_v3"]
    }]
  }'
```

The generated `main.nf` stages the S3 directories with `file(params.x)` →
`path(...)`, which `BUILD_ENTRY_ARGS` resolves to local staged paths via
`entryargs -fileflat`. Nothing is baked; the reference + fastqs are pulled from
S3 into the run's shared filesystem.

## 5. Results (run `2002042`)

**Scientifically correct — matches 10x's published values for this dataset:**

| Metric | This run | 10x published |
|---|---|---|
| Estimated cells | 1,220 | ~1,222 |
| Mean reads/cell | 54,592 | ~54 K |
| Median genes/cell | 2,008 | ~2,000 |
| Number of reads | 66,601,887 | 66.6 M |
| Valid barcodes | 97.4 % | — |
| Sequencing saturation | 70.8 % | — |
| Reads mapped to genome | 96.1 % | ~96 % |
| Reads mapped confidently | 93.6 % | — |
| Total genes detected | 19,996 | ~20 K |

The 42-file `outs/` tree (matrices, `.h5`, BAM, `.cloupe`, secondary analysis,
`web_summary.html`, ~4 GB) is published to `omics-out/2002042/pubdir/` and
exported to S3.

**Operational profile:**

| | Value |
|---|---|
| Wall time | **1.7 h (102 min)**, 0 task failures |
| Tasks | 131 |
| Peak concurrent tasks | **9** (real alignment-chunk fan-out) |
| Per-task compute | median 19 s; **max ~13 min** (4 alignment stages, 4 cpu / 22 G) |
| DYNAMIC storage peak | **44 GiB** |
| Est. cost | **~$0.38** (proxy rates — verify against HealthOmics pricing) |

Unlike a toy run (whose wall time is dominated by per-task provisioning on a
linear DAG), this real run's wall time is dominated by genuine alignment compute,
and the fan-out (peak 9 concurrent) puts parallelism to work.

## Storage sizing (DYNAMIC → STATIC)

This run **peaked at 44 GiB on DYNAMIC** storage. For repeated production runs of
a similarly-sized sample, size a **STATIC** allocation at ~**55–65 GiB** (peak ×
~1.3 headroom) to trade DYNAMIC's elasticity premium for STATIC's lower per-GiB
rate. The workflow: run once on DYNAMIC (zero sizing risk — it reports the peak),
then set STATIC from the measured peak.

## Notes

- **Scale.** pbmc_1k_v3 (~1k cells / ~66 M reads) is a real but modestly-sized
  sample; customers often run 5k–20k+ cells, which push wall time, storage peak,
  and fan-out further. This is the right *first* real-dataset run; a 10k-cell
  dataset would stress the upper end.
- **Backend.** The run is HealthOmics (managed, shared `/mnt/workflow`). Real
  CellRanger on AWS Batch (isolated `s3://` containers) is currently blocked by
  an object-store abs-path limitation in a fastq-sharding stage — independent of
  the transpiler's orchestration; see `docs/LIVE_AWS_TEST.md`.
