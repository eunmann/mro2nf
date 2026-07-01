#!/usr/bin/env bash
set -euo pipefail

# Run transpiled fixtures LIVE on AWS HealthOmics and verify each is byte-identical
# to real Martian (mrp), IN PARALLEL. Reads the CDK stack outputs, builds+pushes a
# runtime image per fixture, registers one HealthOmics workflow per fixture,
# starts all runs concurrently, polls them to COMPLETED, then diffs each exported
# pipeline_outs.json against a local mrp golden (path-normalized).
#
# HealthOmics is NOT available in every region (e.g. not us-east-2) — deploy the
# CDK stack in a supported region (us-east-1) so ECR/S3/role are colocated.
#
#   Prereqs: `npx cdk deploy` in a HealthOmics region; `aws sso login`; docker;
#            a local Martian build (MARTIAN_BIN); a linux/amd64 mre; zip.
#
#   AWS_PROFILE=default ./test/e2e/aws_healthomics.sh                # default subset
#   AWS_PROFILE=default ./test/e2e/aws_healthomics.sh file_min map_file
#   NO_BUILD=1 ./test/e2e/aws_healthomics.sh file_min                # reuse images
#
# Env: AWS_PROFILE (default), STACK (Mro2nfStack), MARTIAN_BIN, NO_BUILD,
#      POLL_SECS (30), MAX_WAIT_SECS (2400).

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

AWS_PROFILE="${AWS_PROFILE:-default}"; export AWS_PROFILE
STACK="${STACK:-Mro2nfStack}"
MARTIAN_BIN="${MARTIAN_BIN:-$HOME/workdir/martian/bin}"
POLL_SECS="${POLL_SECS:-30}"; MAX_WAIT_SECS="${MAX_WAIT_SECS:-2400}"

command -v aws >/dev/null || { echo "need aws cli"; exit 1; }
command -v docker >/dev/null || { echo "need docker"; exit 1; }
command -v zip >/dev/null || { echo "need zip"; exit 1; }
[ -x "$MARTIAN_BIN/mrp" ] || { echo "need mrp at MARTIAN_BIN=$MARTIAN_BIN"; exit 1; }

FIXTURES=("$@")
# HealthOmics runs are slow (per-run instance provisioning); default to a small
# but representative subset. Pass more explicitly to widen coverage.
[ ${#FIXTURES[@]} -eq 0 ] && FIXTURES=(file_min split_test file_tree map_file)

# The stack region must be known before querying it (HealthOmics regions only).
export AWS_REGION="${AWS_REGION:-us-east-1}"
out() { aws cloudformation describe-stacks --stack-name "$STACK" --region "$AWS_REGION" \
    --query "Stacks[0].Outputs[?OutputKey=='$1'].OutputValue" --output text; }
REGION="$(out Region)"; ECR="$(out EcrRepoUri)"
BUCKET="$(out WorkBucketName)"; OMICS_ROLE="$(out OmicsRoleArn)"
[ -n "$ECR" ] && [ -n "$BUCKET" ] && [ -n "$OMICS_ROLE" ] || { echo "missing stack outputs (deployed in $AWS_REGION?)"; exit 1; }
echo "stack=$STACK region=$REGION bucket=$BUCKET omics_role=$OMICS_ROLE"

MRE="$ROOT/mre-linux"
[ -x "$MRE" ] || GOOS=linux GOARCH=amd64 go build -o "$MRE" ./cmd/mre
SHELL_PY="$ROOT/vendor-martian/python/martian_shell.py"
WORK="$(mktemp -d)"; trap 'rm -rf "$WORK"' EXIT

# --- phase 1: transpile (healthomics) + build + push + package + register ---
[ "${NO_BUILD:-}" != "1" ] && aws ecr get-login-password --region "$REGION" | docker login --username AWS --password-stdin "$ECR" >/dev/null
declare -A WFID RUNID
for fx in "${FIXTURES[@]}"; do
    mrjob=(); [ -f "testdata/$fx/mrjob.sh" ] && mrjob=(-mrjob "testdata/$fx/mrjob.sh")
    ./mro2nf -o "$WORK/$fx" -target healthomics -container "${ECR}:${fx}" \
        -mre "$MRE" -shell "$SHELL_PY" ${mrjob[@]+"${mrjob[@]}"} \
        -mropath "testdata/$fx" "testdata/$fx/pipeline.mro" >"$WORK/$fx.tp.log" 2>&1 || { echo "TRANSPILE_FAIL $fx"; continue; }
    if [ "${NO_BUILD:-}" != "1" ]; then
        ( cd "$WORK/$fx" && docker build --platform linux/amd64 -t "${ECR}:${fx}" . >"$WORK/$fx.build.log" 2>&1 ) || { echo "BUILD_FAIL $fx"; continue; }
        docker push "${ECR}:${fx}" >"$WORK/$fx.push.log" 2>&1 || { echo "PUSH_FAIL $fx"; continue; }
    fi
    ( cd "$WORK/$fx" && bash package.sh >/dev/null 2>&1 )
    WFID[$fx]="$(aws omics create-workflow --engine NEXTFLOW --main main.nf \
        --name "mro2nf-$fx-$$" --definition-zip "fileb://$WORK/$fx/workflow.zip" \
        --parameter-template "file://$WORK/$fx/parameter-template.json" \
        --query id --output text)"
    echo "REGISTERED $fx workflow=${WFID[$fx]}"
done

# Workflows must be ACTIVE before a run can start.
for fx in "${FIXTURES[@]}"; do
    [ -n "${WFID[$fx]:-}" ] || continue
    for _ in $(seq 1 40); do
        st="$(aws omics get-workflow --id "${WFID[$fx]}" --query status --output text 2>/dev/null || true)"
        [ "$st" = "ACTIVE" ] && break
        [ "$st" = "FAILED" ] && { echo "WORKFLOW_FAILED $fx"; break; }
        sleep 5
    done
done

# --- phase 2: start all runs (parallel by nature — HealthOmics schedules them) ---
for fx in "${FIXTURES[@]}"; do
    [ "$(aws omics get-workflow --id "${WFID[$fx]:-x}" --query status --output text 2>/dev/null || true)" = "ACTIVE" ] || continue
    RUNID[$fx]="$(aws omics start-run --workflow-id "${WFID[$fx]}" --role-arn "$OMICS_ROLE" \
        --name "mro2nf-$fx-$$" --output-uri "s3://${BUCKET}/omics-out" \
        --parameters "{\"container\":\"${ECR}:${fx}\"}" --query id --output text)"
    echo "STARTED $fx run=${RUNID[$fx]}"
done

# --- phase 3: poll all to a terminal state ---
deadline=$((SECONDS + MAX_WAIT_SECS))
declare -A DONE
while [ "$SECONDS" -lt "$deadline" ]; do
    pending=0
    for fx in "${FIXTURES[@]}"; do
        [ -n "${RUNID[$fx]:-}" ] || continue
        [ -n "${DONE[$fx]:-}" ] && continue
        st="$(aws omics get-run --id "${RUNID[$fx]}" --query status --output text 2>/dev/null || echo UNKNOWN)"
        case "$st" in
            COMPLETED|FAILED|CANCELLED) DONE[$fx]="$st"; echo "RUN_$st $fx";;
            *) pending=$((pending+1));;
        esac
    done
    [ "$pending" -eq 0 ] && break
    sleep "$POLL_SECS"
done

# --- phase 4: verify each COMPLETED run vs a local mrp golden ---
rc=0
for fx in "${FIXTURES[@]}"; do
    if [ "${DONE[$fx]:-}" != "COMPLETED" ]; then
        echo "FAIL[$fx]: run ${DONE[$fx]:-timeout} (log: aws omics get-run --id ${RUNID[$fx]:-?})"; rc=1; continue
    fi
    mt="$(mktemp -d)"; [ -d "$ROOT/testdata/$fx/input" ] && ln -s "$ROOT/testdata/$fx/input" "$mt/input"
    ( cd "$mt" && MROPATH="$ROOT/testdata/$fx" "$MARTIAN_BIN/mrp" "$ROOT/testdata/$fx/pipeline.mro" mrp \
        --jobmode=local --localcores=2 --localmem=4 --disable-ui --nopreflight >/dev/null 2>&1 ) \
        || { echo "FAIL[$fx]: local mrp"; rc=1; rm -rf "$mt"; continue; }
    mrp_json="$(ls -d "$mt"/mrp/*/fork0/_outs 2>/dev/null | sort | head -1)"
    # HealthOmics exports params.outdir under s3://<output-uri>/<runId>/out/...; find the published file.
    key="$(aws s3 ls "s3://${BUCKET}/omics-out/${RUNID[$fx]}/" --recursive | awk '/pipeline_outs\.json$/{print $4; exit}')"
    if [ -z "$key" ] || ! aws s3 cp "s3://${BUCKET}/${key}" "$mt/nf_outs.json" >/dev/null 2>&1; then
        echo "FAIL[$fx]: no exported pipeline_outs.json"; rc=1; rm -rf "$mt"; continue
    fi
    if python3 "$ROOT/test/e2e/normcmp.py" "$mrp_json" "$mt/mrp/outs" "$mt/nf_outs.json" "$fx" >/dev/null 2>&1; then
        echo "OK[$fx]: AWS HealthOmics pipeline_outs matches mrp"
    else
        echo "FAIL[$fx]: pipeline_outs mismatch"; rc=1
    fi
    rm -rf "$mt"
done
exit "$rc"
