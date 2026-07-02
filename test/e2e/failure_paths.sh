#!/usr/bin/env bash
set -euo pipefail

# Behavioral failure-path checks for the content-based retry contract — the one
# part of the runtime no golden-diff case can reach, because nothing may fail:
#
#   1. assert_min: a stage that calls martian.exit(). The shim must exit 42 and
#      the generated errorStrategy must TERMINATE — exactly one task attempt.
#   2. flaky_retry: a stage that fails (ordinary, retryable) while its memory
#      allocation is 1 GB and succeeds once escalated. The run must succeed
#      with mem_gb == 2 — proof the retry ran AND `memory * task.attempt` grew.
#
# These are behavioral cases (no mrp golden): mrp has no retry policy to
# diff against; what is under test is mre's exit-code contract with Nextflow.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

if ! command -v java >/dev/null 2>&1 && [ -s "$HOME/.sdkman/bin/sdkman-init.sh" ]; then
    set +u
    # shellcheck disable=SC1091
    source "$HOME/.sdkman/bin/sdkman-init.sh"
    set -u
fi

# A missing prerequisite skips locally but FAILS under E2E_REQUIRE=1 (set in
# CI), matching the sibling harnesses.
missing() {
    [ "${E2E_REQUIRE:-0}" = "1" ] && { echo "FAIL[failure_paths]: $1 (required by E2E_REQUIRE=1)"; exit 1; }
    echo "SKIP[failure_paths]: $1"
    exit 0
}
command -v nextflow >/dev/null 2>&1 || missing "nextflow not found"
command -v java >/dev/null 2>&1 || missing "java not found"
command -v python3 >/dev/null 2>&1 || missing "python3 not found"

export NXF_ANSI_LOG=false NXF_DISABLE_CHECK_LATEST=true
export NXF_OPTS="${NXF_OPTS:--Xms256m -Xmx1g -XX:+UseSerialGC}"

make build >/dev/null

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

transpile() { # fixture -> project dir on stdout
    local fx="$1" proj="$WORK/$1"
    ./mro2nf -o "$proj" -mre "$ROOT/mre" \
        -shell "$ROOT/vendor-martian/python/martian_shell.py" \
        -mropath "testdata/$fx" "testdata/$fx/pipeline.mro" >/dev/null
    echo "$proj"
}

# attempts NAME TRACE -> number of task rows whose process name contains NAME.
# The trace has one row per attempt (including failed ones), so this counts
# attempts. Column 4 is the task name in the default trace fields.
attempts() {
    awk -F'\t' -v n="$1" 'NR > 1 && index($4, n) { c++ } END { print c + 0 }' "$2"
}

fail=0

# --- 1. ASSERT terminates without retrying -----------------------------------
proj="$(transpile assert_min)"
if (cd "$proj" && nextflow run main.nf -with-trace trace.txt >run.log 2>&1); then
    echo "FAIL[assert_min]: run succeeded; an assertion must fail the pipeline"
    fail=1
else
    n="$(attempts BOOM "$proj/trace.txt")"
    if [ "$n" != "1" ]; then
        echo "FAIL[assert_min]: $n BOOM attempts (an ASSERT must terminate, not retry)"
        sed 's/^/    /' "$proj/trace.txt"
        fail=1
    elif ! awk -F'\t' 'NR > 1 && index($4, "BOOM") && $6 == 42 { found = 1 } END { exit !found }' "$proj/trace.txt"; then
        echo "FAIL[assert_min]: BOOM did not exit 42 (the ASSERT exit-code contract)"
        sed 's/^/    /' "$proj/trace.txt"
        fail=1
    else
        echo "OK[assert_min]: ASSERT terminated after 1 attempt with exit 42"
    fi
fi

# --- 2. Ordinary failure retries with escalated memory -----------------------
proj="$(transpile flaky_retry)"
if ! (cd "$proj" && nextflow run main.nf -with-trace trace.txt >run.log 2>&1); then
    echo "FAIL[flaky_retry]: run failed; the retry should have recovered it"
    tail -5 "$proj/run.log" | sed 's/^/    /'
    fail=1
else
    got="$(python3 -c "import json; print(json.load(open('$proj/results/pipeline_outs.json'))['mem_gb'])")"
    n="$(attempts FLAKY "$proj/trace.txt")"
    if [ "$got" != "2.0" ] && [ "$got" != "2" ]; then
        echo "FAIL[flaky_retry]: mem_gb=$got, want 2 (attempt-2 escalated allocation)"
        fail=1
    elif [ "$n" != "2" ]; then
        echo "FAIL[flaky_retry]: $n FLAKY attempts, want 2 (fail once, succeed on retry)"
        sed 's/^/    /' "$proj/trace.txt"
        fail=1
    else
        echo "OK[flaky_retry]: retried once and succeeded with escalated memory (mem_gb=$got)"
    fi
fi

exit "$fail"
