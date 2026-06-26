#!/usr/bin/env bash
set -euo pipefail

# End-to-end differential tests: transpile each pipeline, run it under Nextflow,
# and assert the result matches the committed golden mrp output.

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

# run_case <name> <mro-dir> <golden-file>
run_case() {
    local name="$1" mrodir="$2" golden="$3"
    local proj
    proj="$(mktemp -d)"

    ./mart -o "$proj" \
        -mre "$ROOT/mre" \
        -shell "$ROOT/vendor-martian/python/martian_shell.py" \
        -mropath "$mrodir" \
        "$mrodir/pipeline.mro" >/dev/null

    (cd "$proj" && nextflow run main.nf >/dev/null)

    python3 - "$proj/results/pipeline_outs.json" "$golden" "$name" <<'PY'
import json, sys
got = json.load(open(sys.argv[1]))
gold = json.load(open(sys.argv[2]))
if got != gold:
    print(f"FAIL[{sys.argv[3]}]: nextflow={got} golden={gold}")
    sys.exit(1)
print(f"OK[{sys.argv[3]}]: {got}")
PY

    rm -rf "$proj"
}

run_case split_test testdata/split_test \
    testdata/split_test/expected/SUM_SQUARE_PIPELINE/fork0/_outs
run_case fork_min testdata/fork_min \
    testdata/fork_min/expected/scale_all_outs.json
run_case struct_min testdata/struct_min \
    testdata/struct_min/expected/stats_pipe_outs.json
run_case modifiers_min testdata/modifiers_min \
    testdata/modifiers_min/expected/top_outs.json
run_case alias_min testdata/alias_min \
    testdata/alias_min/expected/p_outs.json
run_case exec_min testdata/exec_min \
    testdata/exec_min/expected/ep_outs.json
run_case kitchen_sink testdata/kitchen_sink \
    testdata/kitchen_sink/expected/main_outs.json
run_case file_chain testdata/file_chain \
    testdata/file_chain/expected/cp_outs.json

# file_min verifies a published file output: content and basename rewrite. Its
# pipeline_outs.json uses a basename (not an mrp abspath), so it is checked
# directly rather than via run_case.
proj="$(mktemp -d)"
trap 'rm -rf "$proj"' EXIT
./mart -o "$proj" -mre "$ROOT/mre" -shell "$ROOT/vendor-martian/python/martian_shell.py" \
    -mropath testdata/file_min testdata/file_min/pipeline.mro >/dev/null
(cd "$proj" && nextflow run main.nf >/dev/null)
if [ "$(cat "$proj/results/note.txt")" = "x=42" ] &&
    grep -q '"note": "note.txt"' "$proj/results/pipeline_outs.json"; then
    echo "OK[file_min]: published note.txt content + basename rewrite"
else
    echo "FAIL[file_min]"
    exit 1
fi
run_case diamond_min testdata/diamond_min \
    testdata/diamond_min/expected/outs.json
run_case empty_fork_min testdata/empty_fork_min \
    testdata/empty_fork_min/expected/outs.json
run_case stage_entry testdata/stage_entry \
    testdata/stage_entry/expected/outs.json
run_case struct_proj testdata/struct_proj \
    testdata/struct_proj/expected/outs.json
run_case map_fork testdata/map_fork \
    testdata/map_fork/expected/outs.json
