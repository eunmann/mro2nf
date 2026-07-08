#!/usr/bin/env bash
set -euo pipefail

# Run transpiled fixtures LIVE on AWS Batch + S3 and verify each is byte-identical
# to real Martian (mrp), IN PARALLEL. Reads the CDK stack outputs (deploy/awsbatch-cdk),
# builds+pushes one runtime image per fixture (shared cached base layer), launches
# all Nextflow runs concurrently on Batch, then diffs each S3-published
# pipeline_outs.json against a local mrp golden (path-normalized, like mrp_diff).
#
#   Prereqs: `npx cdk deploy` done; `aws sso login --profile $AWS_PROFILE`; docker;
#            nextflow; a local Martian build (MARTIAN_BIN); a linux/amd64 mre.
#
#   AWS_PROFILE=default ./test/e2e/aws_run.sh                 # default fixture set
#   AWS_PROFILE=default ./test/e2e/aws_run.sh file_min map_file
#   NO_BUILD=1 ./test/e2e/aws_run.sh file_min                 # reuse pushed images
#   RUN_PARALLEL=8 ./test/e2e/aws_run.sh ...                  # concurrency cap
#
# Env: AWS_PROFILE (default: default), STACK (Mro2nfStack), MARTIAN_BIN
#      (~/workdir/martian/bin), NO_BUILD, RUN_PARALLEL (6).

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

AWS_PROFILE="${AWS_PROFILE:-default}"
STACK="${STACK:-Mro2nfStack}"
MARTIAN_BIN="${MARTIAN_BIN:-$HOME/workdir/martian/bin}"
RUN_PARALLEL="${RUN_PARALLEL:-6}"
export AWS_PROFILE
export NXF_ANSI_LOG=false NXF_DISABLE_CHECK_LATEST=true
export NXF_OPTS="${NXF_OPTS:--Xms256m -Xmx1g -XX:+UseSerialGC}"

command -v aws >/dev/null || { echo "need aws cli"; exit 1; }
command -v docker >/dev/null || { echo "need docker"; exit 1; }
command -v nextflow >/dev/null || { echo "need nextflow"; exit 1; }
[ -x "$MARTIAN_BIN/mrp" ] || { echo "need mrp at MARTIAN_BIN=$MARTIAN_BIN"; exit 1; }

FIXTURES=("$@")
if [ ${#FIXTURES[@]} -eq 0 ]; then
    # A broad spread across constructs + output shapes (each byte-checked vs mrp).
    FIXTURES=(
        file_min file_chain split_test kitchen_sink file_tree map_file
        struct_of_file dir_out fork_min map_split_file struct_file_array
        map_file_array map_null_map diamond_min struct_min
    )
fi

# --- stack outputs ---
# The stack region must be known before querying it (the profile's default region
# may differ, e.g. HealthOmics-less us-east-2). Override with AWS_REGION if needed.
export AWS_REGION="${AWS_REGION:-us-east-1}"
out() { aws cloudformation describe-stacks --stack-name "$STACK" --region "$AWS_REGION" \
    --query "Stacks[0].Outputs[?OutputKey=='$1'].OutputValue" --output text; }
REGION="$(out Region)"; ECR="$(out EcrRepoUri)"
BUCKET="$(out WorkBucketName)"; QUEUE="$(out BatchJobQueue)"
[ -n "$ECR" ] && [ -n "$BUCKET" ] && [ -n "$QUEUE" ] || { echo "missing stack outputs (deployed in $AWS_REGION?)"; exit 1; }
echo "stack=$STACK region=$REGION bucket=$BUCKET queue=$QUEUE"

# Nextflow's AWS SDK reads static creds more reliably than the SSO cache.
eval "$(aws configure export-credentials --profile "$AWS_PROFILE" --format env)"

MRE="$ROOT/mre-linux"
[ -x "$MRE" ] || { echo "building linux/amd64 mre-linux"; GOOS=linux GOARCH=amd64 go build -o "$MRE" ./cmd/mre; }
SHELL_PY="$ROOT/vendor-martian/python/martian_shell.py"
WORK="$(mktemp -d)"; trap 'rm -rf "$WORK"' EXIT

transpile() {
    local fx="$1" mrjob=(); [ -f "testdata/$fx/mrjob.sh" ] && mrjob=(-mrjob "testdata/$fx/mrjob.sh")
    ./mro2nf -o "$WORK/$fx" -target awsbatch -container "${ECR}:dev-${fx}" \
        -mre "$MRE" -shell "$SHELL_PY" ${mrjob[@]+"${mrjob[@]}"} \
        -mropath "testdata/$fx" "testdata/$fx/pipeline.mro"
}

# --- phase 1: transpile + build + push (parallel) ---
if [ "${NO_BUILD:-}" != "1" ]; then
    aws ecr get-login-password --region "$REGION" | docker login --username AWS --password-stdin "$ECR" >/dev/null
    build_one() {
        # Failures report and return 0 (like run_one): a nonzero return would
        # propagate through bash -c and xargs (exit 123) and, under set -e,
        # abort the whole campaign. The .phase1_fail marker makes the failure
        # sticky: phase 2 must not submit the fixture (the ECR tag may still
        # hold a STALE image from an earlier campaign) and phase 3 must fail
        # it even if stale S3 outputs from a previous run would verify green.
        local fx="$1"
        transpile "$fx" >"$WORK/$fx.tp.log" 2>&1 || { echo "TRANSPILE_FAIL $fx"; touch "$WORK/$fx.phase1_fail"; return 0; }
        ( cd "$WORK/$fx" && docker build --platform linux/amd64 -t "${ECR}:dev-${fx}" . >"$WORK/$fx.build.log" 2>&1 ) || { echo "BUILD_FAIL $fx"; touch "$WORK/$fx.phase1_fail"; return 0; }
        docker push "${ECR}:dev-${fx}" >"$WORK/$fx.push.log" 2>&1 || { echo "PUSH_FAIL $fx"; touch "$WORK/$fx.phase1_fail"; return 0; }
        echo "IMAGE_READY $fx"
    }
    export -f build_one transpile; export ROOT ECR MRE SHELL_PY WORK
    printf '%s\n' "${FIXTURES[@]}" | xargs -P "$RUN_PARALLEL" -I{} bash -c 'build_one "$@"' _ {}
else
    # Guarded like the parallel path: under set -e an unguarded failure would
    # abort the whole harness instead of reporting and moving on.
    for fx in "${FIXTURES[@]}"; do
        transpile "$fx" >"$WORK/$fx.tp.log" 2>&1 \
            || { echo "TRANSPILE_FAIL $fx"; touch "$WORK/$fx.phase1_fail"; }
    done
fi

# --- phase 2: run on Batch (parallel) ---
run_one() {
    local fx="$1"
    # Never submit a fixture whose phase 1 failed: the ECR tag may still hold
    # a stale image from an earlier campaign, and a run against it could
    # publish outputs that verify green against the wrong code.
    [ -e "$WORK/$fx.phase1_fail" ] && { echo "RUN_SKIP $fx (phase 1 failed)"; return 0; }
    ( cd "$WORK/$fx" && nextflow run main.nf \
        --aws_queue "$QUEUE" --aws_region "$AWS_REGION" --container "${ECR}:dev-${fx}" \
        --aws_outdir "s3://${BUCKET}/results/${fx}" \
        -work-dir "s3://${BUCKET}/work/${fx}" >"$WORK/$fx.run.log" 2>&1 ) \
        && echo "RUN_OK $fx" || echo "RUN_FAIL $fx"
}
export -f run_one; export QUEUE BUCKET AWS_REGION ECR WORK
echo "=== launching ${#FIXTURES[@]} Batch runs (parallel=$RUN_PARALLEL) ==="
printf '%s\n' "${FIXTURES[@]}" | xargs -P "$RUN_PARALLEL" -I{} bash -c 'run_one "$@"' _ {}

# --- phase 3: verify each vs a local mrp golden ---
rc=0
for fx in "${FIXTURES[@]}"; do
    # A phase-1 failure fails the fixture unconditionally: stale S3 outputs
    # published by an earlier campaign must never let it verify green.
    if [ -e "$WORK/$fx.phase1_fail" ]; then
        echo "FAIL[$fx]: phase 1 failed (see $WORK/$fx.{tp,build,push}.log)"; rc=1; continue
    fi
    [ -f "$WORK/$fx/main.nf" ] || { echo "FAIL[$fx]: not transpiled"; rc=1; continue; }
    mt="$(mktemp -d)"; [ -d "$ROOT/testdata/$fx/input" ] && ln -s "$ROOT/testdata/$fx/input" "$mt/input"
    if ! ( cd "$mt" && MROPATH="$ROOT/testdata/$fx" "$MARTIAN_BIN/mrp" "$ROOT/testdata/$fx/pipeline.mro" mrp \
            --jobmode=local --localcores=2 --localmem=4 --disable-ui --nopreflight >/dev/null 2>&1 ); then
        echo "FAIL[$fx]: local mrp"; rc=1; rm -rf "$mt"; continue
    fi
    # Guarded: under pipefail a glob miss makes ls (and thus the assignment)
    # nonzero, which would abort the whole verify loop under set -e.
    mrp_json="$(ls -d "$mt"/mrp/*/fork0/_outs 2>/dev/null | sort | head -1)" || true
    if ! aws s3 cp "s3://${BUCKET}/results/${fx}/pipeline_outs.json" "$mt/nf_outs.json" >/dev/null 2>&1; then
        echo "FAIL[$fx]: no S3 pipeline_outs.json (see $WORK/$fx.run.log)"; rc=1; rm -rf "$mt"; continue
    fi
    if python3 "$ROOT/test/e2e/normcmp.py" "$mrp_json" "$mt/mrp/outs" "$mt/nf_outs.json" "$fx" >/dev/null 2>&1; then
        echo "OK[$fx]: AWS Batch pipeline_outs matches mrp"
    else
        echo "FAIL[$fx]: pipeline_outs mismatch (run log: $WORK/$fx.run.log)"
        # normcmp exits nonzero here by construction; without the guard,
        # pipefail + set -e would abort the loop on the first mismatch.
        python3 "$ROOT/test/e2e/normcmp.py" "$mrp_json" "$mt/mrp/outs" "$mt/nf_outs.json" "$fx" | sed 's/^/    /' || true
        rc=1
    fi
    rm -rf "$mt"
done
exit "$rc"
