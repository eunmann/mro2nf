#!/usr/bin/env bash
set -euo pipefail

# Data-movement benchmark harness (epic #18 / issue #17). Transpiles each
# benchmark pipeline, runs the generated Nextflow with Nextflow's own reporting
# (-with-trace, -with-dag), then measures how many times each benchmark's probe
# file is staged across the run's work dir. It turns "not needlessly copying"
# into a number and gates the data-plane work against a per-file transfer
# multiplier (ideal = 1).
#
#   make bench                 # run + compare against bench/baseline.json
#   BENCH_UPDATE=1 make bench   # record the current run as the new baseline
#
# Backend portability: the SAME generated project runs under any executor; only
# the config profile differs. Override to exercise another backend unchanged:
#   BENCH_PROFILE=awsbatch BENCH_WORKDIR=s3://bucket/work make bench
# The default is the local executor so the harness runs in CI without cloud
# credentials; the local `refs` count is the portable stand-in for S3 objects.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

if ! command -v java >/dev/null 2>&1 && [ -s "$HOME/.sdkman/bin/sdkman-init.sh" ]; then
    set +u
    # shellcheck disable=SC1091
    source "$HOME/.sdkman/bin/sdkman-init.sh"
    set -u
fi

# A missing prerequisite skips locally but FAILS under E2E_REQUIRE=1 (set in
# CI), so a runner-image regression cannot turn the gate into a silent skip.
missing() {
    [ "${E2E_REQUIRE:-0}" = "1" ] && { echo "FAIL[bench]: $1 (required by E2E_REQUIRE=1)"; exit 1; }
    echo "SKIP[bench]: $1"
    exit 0
}
command -v nextflow >/dev/null 2>&1 || missing "nextflow not found"
command -v java >/dev/null 2>&1 || missing "java not found"
command -v python3 >/dev/null 2>&1 || missing "python3 not found"

export NXF_ANSI_LOG=false NXF_DISABLE_CHECK_LATEST=true
export NXF_OPTS="${NXF_OPTS:--Xms256m -Xmx1g -XX:+UseSerialGC}"

BENCH_PROFILE="${BENCH_PROFILE:-}"
BENCH_WORKDIR="${BENCH_WORKDIR:-}"

make build >/dev/null

# name  probe-string  producer-count : each benchmark and the marker its probe
# file carries, plus how many stages genuinely PRODUCE that file (denominator of
# the transfer multiplier). Chain: one MAKEBIG produces the file the rest carry.
# Split: one entry staging feeds the payload broadcast to every chunk.
BENCHES=(
    "chain MRE_BENCH_CHAIN_PROBE 1"
    "split MRE_BENCH_SPLIT_PROBE 1"
)

OUT="$(mktemp -d)"
trap "rm -rf '$OUT'" EXIT
METRICS="$OUT/metrics.jsonl"
: >"$METRICS"

run_one() {
    local name="$1" probe="$2" producers="$3" dir="bench/$1"
    [ -f "$dir/pipeline.mro" ] || { echo "SKIP[$name]: no fixture"; return 0; }

    local proj="$OUT/$name"; mkdir -p "$proj"
    if ! ./mro2nf -o "$proj" -mre "$ROOT/mre" \
            -shell "$ROOT/vendor-martian/python/martian_shell.py" \
            -mropath "$dir" "$dir/pipeline.mro" >"$proj/transpile.log" 2>&1; then
        echo "FAIL[$name]: transpile"; sed 's/^/    /' "$proj/transpile.log" | tail -10; return 1
    fi

    local args=(run main.nf -with-trace trace.txt -with-dag dag.mmd)
    [ -n "$BENCH_PROFILE" ] && args+=(-profile "$BENCH_PROFILE")
    [ -n "$BENCH_WORKDIR" ] && args+=(-work-dir "$BENCH_WORKDIR")
    if ! ( cd "$proj" && nextflow "${args[@]}" >nf.log 2>&1 ); then
        echo "FAIL[$name]: nextflow"; sed 's/^/    /' "$proj/nf.log" | tail -12; return 1
    fi

    python3 "$ROOT/test/e2e/bench_metrics.py" "$name" "$proj/trace.txt" \
        "$proj" "$probe" "$producers" "$proj/dag.mmd" >>"$METRICS"
}

rc=0
for spec in "${BENCHES[@]}"; do
    # shellcheck disable=SC2086
    set -- $spec
    run_one "$1" "$2" "$3" || rc=1
done
[ "$rc" -eq 0 ] || exit "$rc"

python3 "$ROOT/test/e2e/bench_report.py" "$METRICS" "$ROOT/bench/baseline.json"
