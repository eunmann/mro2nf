# Live AWS test infrastructure (CDK)

Minimal, self-contained AWS CDK app that provisions exactly what's needed to run
a transpiled Martian→Nextflow pipeline live, for **both** cloud targets:

| Resource | Used by |
|---|---|
| S3 bucket (work dir + outputs) | `awsbatch` + `healthomics` |
| ECR repo `mro2nf-runtime` (the runtime image and the pipeline images built FROM it) | both |
| VPC (public subnets, S3 gateway endpoint, no NAT) | `awsbatch` |
| Batch compute environment + job queue + instance role, its instances mounting the input buckets at `/mnt/s3` | `awsbatch` |
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
npx cdk deploy -c mountBuckets=<input-bucket>,<reference-bucket>   # prints the outputs below
```

Outputs: `Region`, `WorkBucketName`, `EcrRepoUri`, `BatchJobQueue`, `OmicsRoleArn`.

`mountBuckets` names the buckets holding the pipeline's inputs, comma-separated;
the work bucket is always mounted, so `-c mountBuckets=...` can be left out when
the inputs live there. A run reads the inputs of a mounted bucket in place,
sparing their downloads, and a stage that records an input's path for
another task to open needs it. The instance role may read only the mounted
buckets, so every bucket holding an input belongs in `mountBuckets`: a run
would stage an input of any other bucket and fail to download it. Each Batch instance installs
[Mountpoint for S3](https://github.com/awslabs/mountpoint-s3) at boot, the
release `MOUNTPOINT_VERSION` in `lib/stack.ts` names, checked against the
rpm's SHA-256 pinned beside it, and
mounts every one of them read-only at `/mnt/s3/<bucket>` (`S3_MOUNT_ROOT` in
`lib/stack.ts`), caching up to `mountCacheMiB` (100 GiB) of each bucket's reads
on the instance's root volume; the instance role may read them. The compute
environment runs Amazon Linux 2023, whose `dnf` installs Mountpoint. Deploy
again with a new list to change it: the compute environment then launches its
new instances from the new launch template version.

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
working dir, and the work dir expires after 14 days. Pass entry files as
`s3://` URIs (a `-params-file` or `--<input>`); the `awsbatch` profile refuses
a head-node path. The profile reads an input of a mounted bucket in place
through the mount: a stage receives `s3://<bucket>/<key>` as
`/mnt/s3/<bucket>/<key>`, the same path in every task, as `mrp` passes the
path it was given, and no task downloads it. An input of a bucket the
instances do not mount, neither the work bucket nor one in `mountBuckets`, is
one this stack's instance role may not read: the run stages it, as on every
other profile, and its download fails with AccessDenied. Every input bucket
belongs in `mountBuckets`. The mount is read-only.

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
