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
  days and incomplete uploads are aborted; ECR keeps only the 5 newest images.
  Idle storage is a few hundred MB (the image) ⇒ cents/month.
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
    --outdir s3://<WorkBucketName>/results
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
