#!/usr/bin/env bash
set -euo pipefail

# End-to-end differential test: transpile the split_test pipeline, run it under
# Nextflow, and assert the result matches the committed golden mrp output.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

# Nextflow needs Java; load it via SDKMAN if it is not already on PATH.
if ! command -v java >/dev/null 2>&1 && [ -s "$HOME/.sdkman/bin/sdkman-init.sh" ]; then
    set +u # SDKMAN's init script references unset variables.
    # shellcheck disable=SC1091
    source "$HOME/.sdkman/bin/sdkman-init.sh"
    set -u
fi

command -v nextflow >/dev/null 2>&1 || { echo "SKIP: nextflow not found"; exit 0; }
command -v java >/dev/null 2>&1 || { echo "SKIP: java not found"; exit 0; }
command -v python3 >/dev/null 2>&1 || { echo "SKIP: python3 not found"; exit 0; }

make build >/dev/null

proj="$(mktemp -d)"
trap 'rm -rf "$proj"' EXIT

./mart -o "$proj" \
    -mre "$ROOT/mre" \
    -shell "$ROOT/vendor-martian/python/martian_shell.py" \
    -mropath testdata/split_test \
    testdata/split_test/pipeline.mro

(cd "$proj" && nextflow run main.nf >/dev/null)

python3 - "$proj/results/pipeline_outs.json" \
    "$ROOT/testdata/split_test/expected/SUM_SQUARE_PIPELINE/fork0/_outs" <<'PY'
import json, sys
got = json.load(open(sys.argv[1]))
gold = json.load(open(sys.argv[2]))
if got != gold:
    print(f"FAIL: nextflow={got} golden={gold}")
    sys.exit(1)
print(f"OK: nextflow output matches golden mrp output: {got}")
PY
