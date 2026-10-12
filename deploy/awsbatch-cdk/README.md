# Live AWS test infrastructure (CDK)

Minimal, self-contained AWS CDK app that provisions exactly what's needed to run
a transpiled Martian→Nextflow pipeline live, for **both** cloud targets:

| Resource | Used by |
|---|---|
| S3 bucket (work dir + outputs) | `awsbatch` + `healthomics` |
| ECR repo `mro2nf-runtime` (the runtime image and the pipeline images built FROM it) | both |
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
- **Storage is bounded.** Objects under S3 `work/` (Nextflow's intermediate
  work dir) expire after 14 days and incomplete uploads are aborted; ECR keeps
  only the 5 newest runtime images (tags starting `runtime`, below) and the 20
  newest pipeline images, so pipeline pushes never expire the runtime image.
  Final
  outputs are kept until you delete them or run `cdk destroy`: `results/` (the
  Batch `--outdir` below) and
  `omics-out/` (the HealthOmics `--output-uri`: each run's `pubdir/` outputs
  and logs). Idle storage is a few hundred MB (the image) plus your outputs.
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
- Docker (to build the images) and Nextflow (for the Batch run).

## Deploy

```bash
cd deploy/awsbatch-cdk
npm install
npx cdk bootstrap          # once per account/region
npx cdk deploy             # prints the outputs below
```

Outputs: `Region`, `WorkBucketName`, `EcrRepoUri`, `BatchJobQueue`, `OmicsRoleArn`.

## Run on AWS Batch + S3

One transpiled project runs on every target; `-profile` chooses it
(`docs/TRANSPILER.md` §9), every task in the one image `--mro2nf_image`
names: the pipeline's image, built FROM the runtime image
(`deploy/runtime/Dockerfile`) with the pipeline's code at the paths it was
transpiled from (the README's "Container image").

```bash
# 1. transpile (the same project serves Batch and HealthOmics)
./mro2nf -o out testdata/split_test/pipeline.mro

# 2. build + push the runtime image, then the pipeline's image FROM it with
#    the fixture at the path step 1 transpiled it from (linux/amd64 compute)
aws ecr get-login-password --region <Region> | docker login --username AWS --password-stdin <EcrRepoUri>
docker build --platform linux/amd64 -t <EcrRepoUri>:runtime -f deploy/runtime/Dockerfile .
docker push <EcrRepoUri>:runtime
cat > split_test.Dockerfile <<EOF
FROM <EcrRepoUri>:runtime
COPY . $PWD/testdata/split_test/
EOF
docker build --platform linux/amd64 -t <EcrRepoUri>:split_test -f split_test.Dockerfile testdata/split_test
docker push <EcrRepoUri>:split_test

# 3. run; a site config names the queue and region
cd out
cat > batch.config <<'EOF'
process.queue = '<BatchJobQueue>'
aws.region = '<Region>'
EOF
nextflow run main.nf -profile awsbatch -c batch.config \
    --mro2nf_image <EcrRepoUri>:split_test \
    -work-dir s3://<WorkBucketName>/work \
    --outdir s3://<WorkBucketName>/results
```

Keep `--outdir` on S3: the default `results` publishes to the head node's
working dir, and the work dir expires after 14 days. Entry files are staged
from S3: pass them as `s3://` URIs (a `-params-file` or `--<input>`); the
`awsbatch` profile refuses a head-node path.

## Run on AWS HealthOmics

```bash
# 1-2. transpile and push the pipeline's image, as for Batch
# 3. package with the image pinned by digest, then register + run
cd out
IMAGE=$(docker inspect --format '{{index .RepoDigests 0}}' <EcrRepoUri>:split_test)
./package.sh "$IMAGE"   # builds workflow.zip
WF=$(aws omics create-workflow --engine NEXTFLOW --main main.nf \
    --definition-zip fileb://workflow.zip \
    --parameter-template file://parameter-template.json \
    --query 'id' --output text)
aws omics start-run --workflow-id "$WF" --role-arn <OmicsRoleArn> \
    --output-uri s3://<WorkBucketName>/omics-out
```

The workflow pins the image, so every run executes the code it was
validated with; the parameter template declares only the pipeline's inputs
and `mro2nf_overrides`, so a run cannot name another image. See
[`docs/RUNNING.md`](../../docs/RUNNING.md) for the launch parameters and the
other profiles.

## Teardown

```bash
npx cdk destroy
```
