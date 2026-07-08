# Live AWS test infrastructure (CDK)

Minimal, self-contained AWS CDK app that provisions exactly what's needed to run
a transpiled Martian→Nextflow pipeline live, for **both** cloud targets:

| Resource | Used by |
|---|---|
| S3 bucket (work dir + outputs) | `awsbatch` + `healthomics` |
| ECR repo `mro2nf-runtime` (the image built from the generated `Dockerfile`) | both |
| VPC (public subnets, S3 gateway endpoint, no NAT) | `awsbatch` |
| Batch compute environment + job queue + instance role | `awsbatch` |
| HealthOmics service role + ECR pull policy | `healthomics` |

It's an **example**, not a hardened deployment (public subnets, spot instances,
`DESTROY` removal policies for easy teardown). `cdk destroy` removes everything.

## Cost

Designed to cost **≈ $0 while idle** and only a few cents per test run:

- **No always-on compute.** Batch runs `minvCpus: 0`, so it keeps **zero
  instances between runs** and scales up only while a job is queued, on **spot**
  (~70% cheaper). A tiny test pipeline runs in minutes on 1–2 vCPUs → cents.
- **No NAT gateway** (~$32/mo each) and **no EFS/Lustre** — public subnets + a
  free S3 gateway endpoint are used instead. (Nextflow uses S3 as its work dir,
  so unlike the MRP/Batch pattern it needs no shared filesystem.)
- **Storage is bounded.** S3 `work/` and `omics-out/` objects expire after 14
  days and incomplete uploads are aborted; ECR keeps only the 20 newest images.
  Idle storage is a few hundred MB (the image) ⇒ cents/month. Each Batch instance
  gets a 100 GB root volume (room for the cached image + task scratch, see below);
  it is deleted with the spot instance, so it adds **no idle cost**.
- **HealthOmics** has no idle cost — you're billed per run (omics instance-hours
  + run storage) only while a run executes. Keep the default **DYNAMIC** run
  storage; do **not** request STATIC (it provisions ≥1,200 GiB of Lustre and is
  expensive). A tiny test run is well under a dollar.

The only knobs that could raise the bill are user-driven: launching very large
pipelines (capped here at `maxvCpus: 256`, so a wide parallel run is not vCPU
starved; still $0 idle on spot + `minvCpus: 0`), or leaving data in S3/ECR. Run
`cdk destroy` when done to remove everything.

## Prerequisites

- Node 18+ and the AWS CDK: `npm i -g aws-cdk` (or use the local `npx cdk`).
- AWS credentials in your environment (the live test credentials).
- Docker (to build the runtime image) and Nextflow (for the Batch run).

## Deploy

```bash
cd deploy/awsbatch-cdk
npm install
npx cdk bootstrap          # once per account/region
npx cdk deploy             # prints the outputs below
```

Outputs: `Region`, `WorkBucketName`, `EcrRepoUri`, `BatchJobQueue`, `OmicsRoleArn`.

## Image tag immutability

The ECR repo is created **`IMMUTABLE_WITH_EXCLUSION`**: once a tag is pushed it
cannot be overwritten, so a run that pins `repo:tag` (or its resolved digest) can
never have a *different* image pushed under that tag mid-flight or before its next
run. The `dev-*` prefix is **exempted** (stays mutable) for iterative work — the
e2e harnesses (`test/e2e/aws_run.sh`, `aws_healthomics.sh`) re-push a `dev-<fixture>`
tag every campaign. So: name throwaway/iteration images `dev-…`; use a plain
(immutable) tag or an `@sha256:` digest for anything you want reproducible.

`mro2nf` also **warns at transpile time** when `-container`/`-container-dataplane`
is a mutable tag rather than an `@sha256:` digest on a cloud target — HealthOmics
pins the digest at run start (a running run is safe), but AWS Batch resolves the
tag per task, so pin a digest for the strongest guarantee.

## Instance & image reuse (cutting per-task startup)

The dominant cost of a many-small-task run on an elastic cluster is per-task
startup: acquiring an instance and **pulling the container image again for every
task**. A big runtime image (CellRanger's is ~2.8 GB) makes that the wall-time
bottleneck. The Batch compute environment mitigates it **without any warm-instance
idle cost**:

- A **launch template** sets `ECS_IMAGE_PULL_BEHAVIOR=prefer-cached` (plus cache-
  retention vars) in `/etc/ecs/ecs.config`, so a task on a **reused** instance uses
  the locally cached image instead of re-pulling. Paired with the 100 GB root volume
  so the cache survives between jobs on that instance.
- `allocationStrategy: SPOT_PRICE_CAPACITY_OPTIMIZED` — the deepest, cheapest spot
  pools (fewest interruptions).
- `minvCpus: 0` is kept — **no warm floor, $0 idle**. So instance + image-cache reuse
  happens **within a burst** of jobs (Batch packs them onto running instances), not
  across idle gaps. Raise `minvCpus` only if cross-run warmth matters more than idle
  cost.

Measured on a ~24-task run with a 1.36 GB image: the image was pulled **once per
instance** (ECS `pullStartedAt`/`pullStoppedAt` show a pull only for the first task
on each instance; the rest report *no pull event*), so 24 tasks pulled twice, not 24
times. This is the reuse **AWS HealthOmics cannot do** — its managed compute gives
every task a fresh instance and re-pulls, with no launch-template / AMI / `ecs.config`
control. On a big-image pipeline that difference is most of the wall-time gap between
the two backends.

## Run on AWS Batch + S3

```bash
# 1. transpile for awsbatch (bakes /opt/mro2nf paths + Dockerfile + runtime/ context)
./mro2nf -o out -target awsbatch -container <EcrRepoUri>:latest \
    -mre ./mre -shell ./vendor-martian/python/martian_shell.py \
    -mropath testdata/split_test testdata/split_test/pipeline.mro

# 2. build + push the runtime image
cd out
aws ecr get-login-password --region <Region> | docker login --username AWS --password-stdin <EcrRepoUri>
docker build --platform linux/amd64 -t <EcrRepoUri>:latest .
docker push <EcrRepoUri>:latest

# 3. run (override inputs with a -params-file if desired)
nextflow run main.nf \
    --aws_queue <BatchJobQueue> --aws_region <Region> \
    --container <EcrRepoUri>:latest \
    -work-dir s3://<WorkBucketName>/work \
    --aws_outdir s3://<WorkBucketName>/results
```

## Run on AWS HealthOmics

```bash
# 1. transpile for healthomics (also emits parameter-template.json + package.sh)
./mro2nf -o out -target healthomics -container <EcrRepoUri>:latest \
    -mre ./mre -shell ./vendor-martian/python/martian_shell.py \
    -mropath testdata/split_test testdata/split_test/pipeline.mro

# 2. build + push the image (same as Batch step 2)
# 3. package + register + run
cd out && bash package.sh   # builds workflow.zip
WF=$(aws omics create-workflow --engine NEXTFLOW --main main.nf \
    --definition-zip fileb://workflow.zip \
    --parameter-template file://parameter-template.json \
    --query 'id' --output text)
aws omics start-run --workflow-id "$WF" --role-arn <OmicsRoleArn> \
    --output-uri s3://<WorkBucketName>/omics-out \
    --parameters '{"container":"<EcrRepoUri>:latest"}'
```

> Provide a **linux/amd64** `mre` (`GOOS=linux GOARCH=amd64 go build -o mre ./cmd/mre`).

## Teardown

```bash
npx cdk destroy
```
