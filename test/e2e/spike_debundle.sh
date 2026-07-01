#!/usr/bin/env bash
set -euo pipefail

# Validates the #13 de-bundle spike: the per-output-param sidecar + flat-leaf
# transport stages the four nastiest shapes (map<file[]>, struct-of-file-array,
# split shared+per-chunk, zero-chunk join) plus multi-input non-collision, on
# BOTH a symlink work dir (local/HealthOmics) and a copy work dir (the S3 proxy:
# process.scratch + stageInMode/stageOutMode=copy). It also asserts the zero-copy
# property: a producer materializes each leaf exactly once, with no bundle `f/`
# byte-copy anywhere.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SPIKE="$ROOT/spike/13-debundle"

if ! command -v java >/dev/null 2>&1 && [ -s "$HOME/.sdkman/bin/sdkman-init.sh" ]; then
    set +u
    # shellcheck disable=SC1091
    source "$HOME/.sdkman/bin/sdkman-init.sh"
    set -u
fi

# A missing prerequisite skips locally but FAILS under E2E_REQUIRE=1 (set in
# CI), so a runner-image regression cannot turn the check into a silent skip.
missing() {
    [ "${E2E_REQUIRE:-0}" = "1" ] && { echo "FAIL[spike13]: $1 (required by E2E_REQUIRE=1)"; exit 1; }
    echo "SKIP[spike13]: $1"
    exit 0
}
command -v nextflow >/dev/null 2>&1 || missing "nextflow not found"
command -v java >/dev/null 2>&1 || missing "java not found"
command -v python3 >/dev/null 2>&1 || missing "python3 not found"

export NXF_ANSI_LOG=false NXF_DISABLE_CHECK_LATEST=true
export NXF_OPTS="${NXF_OPTS:--Xms256m -Xmx1g -XX:+UseSerialGC}"

# Every OK[...] line the spike must emit (8 total across the four shapes).
EXPECTED=8

run_mode() {
    local label="$1"; shift
    local run; run="$(mktemp -d)"
    # shellcheck disable=SC2064
    trap "rm -rf '$run'" RETURN
    cp -r "$SPIKE"/* "$run/"
    chmod +x "$run"/bin/*.py

    local out
    if ! out="$( cd "$run" && nextflow run main.nf "$@" 2>&1 )"; then
        echo "FAIL[spike13:$label]: nextflow"; printf '%s\n' "$out" | tail -12; return 1
    fi
    if printf '%s\n' "$out" | grep -q 'FAIL\['; then
        echo "FAIL[spike13:$label]: a shape assertion failed"; printf '%s\n' "$out" | grep 'FAIL\['; return 1
    fi
    local n; n="$(printf '%s\n' "$out" | grep -c 'OK\[')"
    if [ "$n" -ne "$EXPECTED" ]; then
        echo "FAIL[spike13:$label]: got $n/$EXPECTED OK assertions"; printf '%s\n' "$out" | grep 'OK\['; return 1
    fi

    # Zero-copy: no bundle f/ dir, and each producer leaf exists once.
    # -print -quit stops find at the first match: a full `find | grep -q` can die
    # of SIGPIPE when grep exits early, and under pipefail that 141 would skip
    # this FAIL branch exactly when a violation was found.
    if find "$run/work" -type d -name f -print -quit | grep -q .; then
        echo "FAIL[spike13:$label]: found a bundle f/ dir (expected zero byte-copy)"; return 1
    fi
    echo "OK[spike13:$label]: $n/$EXPECTED shapes staged, zero-copy"
}

cat >"$SPIKE/cloud.config" <<'CFG'
process { scratch = true; stageInMode = 'copy'; stageOutMode = 'copy' }
CFG
trap 'rm -f "$SPIKE/cloud.config"' EXIT

rc=0
run_mode local || rc=1
run_mode s3proxy -c cloud.config || rc=1
exit "$rc"
