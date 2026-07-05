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
#      POLL_SECS (30), MAX_WAIT_SECS (2400), KEEP_RESOURCES (1 = skip teardown).
#
# Teardown: on exit the harness deletes every workflow it registered and every
# run that verified OK, so a multi-fixture campaign does not accrue HealthOmics
# resources. Failed/unverified runs (and their workflows, when deletion
# conflicts) are kept for debugging and listed; KEEP_RESOURCES=1 keeps
# everything. See docs/LIVE_AWS_TEST.md for the bulk teardown of leftovers.

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
WORK="$(mktemp -d)"

declare -A WFID RUNID DONE VERIFIED

# Delete what this invocation created: runs that verified OK, then every
# registered workflow (runs must go first — a workflow with live runs rejects
# deletion). Anything kept (failed/unverified runs, conflicting workflows) is
# listed so the operator can debug and then clean up manually.
cleanup() {
    rm -rf "$WORK"
    if [ "${KEEP_RESOURCES:-}" = "1" ]; then
        echo "KEEP_RESOURCES=1: leaving HealthOmics workflows/runs in place"
        return
    fi
    local fx
    for fx in "${!RUNID[@]}"; do
        if [ "${VERIFIED[$fx]:-}" = "1" ]; then
            aws omics delete-run --id "${RUNID[$fx]}" >/dev/null 2>&1 \
                && echo "DELETED run ${RUNID[$fx]} ($fx)" \
                || echo "KEPT run ${RUNID[$fx]} ($fx): delete failed"
        else
            echo "KEPT run ${RUNID[$fx]} ($fx) for debugging (${DONE[$fx]:-not finished})"
        fi
    done
    for fx in "${!WFID[@]}"; do
        aws omics delete-workflow --id "${WFID[$fx]}" >/dev/null 2>&1 \
            && echo "DELETED workflow ${WFID[$fx]} ($fx)" \
            || echo "KEPT workflow ${WFID[$fx]} ($fx): delete failed (live run?)"
    done
}
trap cleanup EXIT

# --- phase 1: transpile (healthomics) + build + push + package + register ---
[ "${NO_BUILD:-}" != "1" ] && aws ecr get-login-password --region "$REGION" | docker login --username AWS --password-stdin "$ECR" >/dev/null
for fx in "${FIXTURES[@]}"; do
    mrjob=(); [ -f "testdata/$fx/mrjob.sh" ] && mrjob=(-mrjob "testdata/$fx/mrjob.sh")
    ./mro2nf -o "$WORK/$fx" -target healthomics -container "${ECR}:${fx}" \
        -mre "$MRE" -shell "$SHELL_PY" ${mrjob[@]+"${mrjob[@]}"} \
        -mropath "testdata/$fx" "testdata/$fx/pipeline.mro" >"$WORK/$fx.tp.log" 2>&1 || { echo "TRANSPILE_FAIL $fx"; continue; }
    if [ "${NO_BUILD:-}" != "1" ]; then
        ( cd "$WORK/$fx" && docker build --platform linux/amd64 -t "${ECR}:${fx}" . >"$WORK/$fx.build.log" 2>&1 ) || { echo "BUILD_FAIL $fx"; continue; }
        docker push "${ECR}:${fx}" >"$WORK/$fx.push.log" 2>&1 || { echo "PUSH_FAIL $fx"; continue; }
    fi
    # Guarded like the build/push steps: under set -e an unguarded failure here
    # would abort the whole harness instead of reporting and moving on.
    ( cd "$WORK/$fx" && bash package.sh >/dev/null 2>&1 ) || { echo "PACKAGE_FAIL $fx"; continue; }
    # Assign the array key only on success: `WFID[$fx]="$(...)" || continue`
    # would create an empty-valued key before the guard fires, and the EXIT
    # cleanup trap would then print a bogus "KEPT workflow  ($fx)" entry.
    wfid="$(aws omics create-workflow --engine NEXTFLOW --main main.nf \
        --name "mro2nf-$fx-$$" --definition-zip "fileb://$WORK/$fx/workflow.zip" \
        --parameter-template "file://$WORK/$fx/parameter-template.json" \
        --query id --output text)" || { echo "REGISTER_FAIL $fx"; continue; }
    WFID[$fx]="$wfid"
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
    # Assign the array key only on success (see WFID above): an empty-valued
    # RUNID[$fx] would make the cleanup trap print a bogus "KEPT run" entry.
    runid="$(aws omics start-run --workflow-id "${WFID[$fx]}" --role-arn "$OMICS_ROLE" \
        --name "mro2nf-$fx-$$" --output-uri "s3://${BUCKET}/omics-out" \
        --parameters "{\"container\":\"${ECR}:${fx}\"}" --query id --output text)" \
        || { echo "START_FAIL $fx"; continue; }
    RUNID[$fx]="$runid"
    echo "STARTED $fx run=${RUNID[$fx]}"
done

# --- phase 3: poll all to a terminal state ---
deadline=$((SECONDS + MAX_WAIT_SECS))
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
    # Guarded: under pipefail a glob miss makes ls (and thus the assignment)
    # nonzero, which would abort the whole verify loop under set -e.
    mrp_json="$(ls -d "$mt"/mrp/*/fork0/_outs 2>/dev/null | sort | head -1)" || true
    # HealthOmics exports params.outdir under s3://<output-uri>/<runId>/out/...; find the published file.
    # aws s3 ls exits nonzero on an empty prefix; the -z check below reports it.
    key="$(aws s3 ls "s3://${BUCKET}/omics-out/${RUNID[$fx]}/" --recursive | awk '/pipeline_outs\.json$/{print $4; exit}')" || true
    if [ -z "$key" ] || ! aws s3 cp "s3://${BUCKET}/${key}" "$mt/nf_outs.json" >/dev/null 2>&1; then
        echo "FAIL[$fx]: no exported pipeline_outs.json"; rc=1; rm -rf "$mt"; continue
    fi
    if python3 "$ROOT/test/e2e/normcmp.py" "$mrp_json" "$mt/mrp/outs" "$mt/nf_outs.json" "$fx" >/dev/null 2>&1; then
        echo "OK[$fx]: AWS HealthOmics pipeline_outs matches mrp"; VERIFIED[$fx]=1
    else
        echo "FAIL[$fx]: pipeline_outs mismatch"; rc=1
    fi
    rm -rf "$mt"
done
exit "$rc"
