#!/usr/bin/env bash
set -euo pipefail

# End-to-end differential tests: transpile each pipeline, run it under Nextflow,
# and assert the result matches the committed golden mrp output.
#
# Cases run in parallel (bounded by E2E_PARALLEL, default 6) because each case is
# an independent `nextflow run` and JVM startup dominates. Re-invokes itself in
# `--one` mode per case so a standard `xargs -P` pool gives the concurrency.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

# Nextflow needs Java; load it via SDKMAN if it is not already on PATH.
if ! command -v java >/dev/null 2>&1 && [ -s "$HOME/.sdkman/bin/sdkman-init.sh" ]; then
    set +u # SDKMAN's init script references unset variables.
    # shellcheck disable=SC1091
    source "$HOME/.sdkman/bin/sdkman-init.sh"
    set -u
fi

# Speed: skip the network version check, drop ANSI redraw, cap the JVM heap so
# many parallel runs stay light. (Inherited by the --one children via the env.)
export NXF_ANSI_LOG=false
export NXF_DISABLE_CHECK_LATEST=true
export NXF_OPTS="${NXF_OPTS:--Xms256m -Xmx1g -XX:+UseSerialGC}"

# --- single-case worker: bash run.sh --one "name|mro-dir|golden" ---
if [[ "${1:-}" == "--one" ]]; then
    IFS='|' read -r name dir golden <<<"$2"
    proj="$(mktemp -d)"
    trap 'rm -rf "$proj"' EXIT

    ./mart -o "$proj" \
        -mre "$ROOT/mre" \
        -shell "$ROOT/vendor-martian/python/martian_shell.py" \
        -mropath "$dir" \
        "$dir/pipeline.mro" >/dev/null 2>&1 || { echo "FAIL[$name]: transpile"; exit 1; }

    (cd "$proj" && nextflow run main.nf >/dev/null 2>&1) || { echo "FAIL[$name]: nextflow"; exit 1; }

    # file_min additionally verifies the published file's content.
    if [[ "$name" == "file_min" ]]; then
        [[ "$(cat "$proj/results/note.txt" 2>/dev/null)" == "x=42" ]] ||
            { echo "FAIL[$name]: note.txt content"; exit 1; }
    fi

    python3 - "$proj/results/pipeline_outs.json" "$golden" "$name" <<'PY'
import json, sys
got = json.load(open(sys.argv[1]))
gold = json.load(open(sys.argv[2]))
ok = got == gold
print(("OK" if ok else "FAIL") + f"[{sys.argv[3]}]: {json.dumps(got, sort_keys=True)}")
sys.exit(0 if ok else 1)
PY
    exit $?
fi

# --- driver ---
command -v nextflow >/dev/null 2>&1 || { echo "SKIP: nextflow not found"; exit 0; }
command -v java >/dev/null 2>&1 || { echo "SKIP: java not found"; exit 0; }
command -v python3 >/dev/null 2>&1 || { echo "SKIP: python3 not found"; exit 0; }

make build >/dev/null

CASES=(
    "split_test|testdata/split_test|testdata/split_test/expected/SUM_SQUARE_PIPELINE/fork0/_outs"
    "fork_min|testdata/fork_min|testdata/fork_min/expected/scale_all_outs.json"
    "struct_min|testdata/struct_min|testdata/struct_min/expected/stats_pipe_outs.json"
    "modifiers_min|testdata/modifiers_min|testdata/modifiers_min/expected/top_outs.json"
    "alias_min|testdata/alias_min|testdata/alias_min/expected/p_outs.json"
    "exec_min|testdata/exec_min|testdata/exec_min/expected/ep_outs.json"
    "kitchen_sink|testdata/kitchen_sink|testdata/kitchen_sink/expected/main_outs.json"
    "file_chain|testdata/file_chain|testdata/file_chain/expected/cp_outs.json"
    "file_min|testdata/file_min|testdata/file_min/expected/outs.json"
    "diamond_min|testdata/diamond_min|testdata/diamond_min/expected/outs.json"
    "empty_fork_min|testdata/empty_fork_min|testdata/empty_fork_min/expected/outs.json"
    "stage_entry|testdata/stage_entry|testdata/stage_entry/expected/outs.json"
    "struct_proj|testdata/struct_proj|testdata/struct_proj/expected/outs.json"
    "map_fork|testdata/map_fork|testdata/map_fork/expected/outs.json"
    "map_split|testdata/map_split|testdata/map_split/expected/outs.json"
    "map_pipe|testdata/map_pipe|testdata/map_pipe/expected/outs.json"
    "map_file|testdata/map_file|testdata/map_file/expected/outs.json"
    "map_pipe_split|testdata/map_pipe_split|testdata/map_pipe_split/expected/outs.json"
    "map_file_keyed|testdata/map_file_keyed|testdata/map_file_keyed/expected/outs.json"
    "struct_of_file|testdata/struct_of_file|testdata/struct_of_file/expected/outs.json"
    "literals_edge|testdata/literals_edge|testdata/literals_edge/expected/outs.json"
    "dir_out|testdata/dir_out|testdata/dir_out/expected/outs.json"
    "api_smoke|testdata/api_smoke|testdata/api_smoke/expected/outs.json"
    "float_to_int|testdata/float_to_int|testdata/float_to_int/expected/outs.json"
    "include_test|testdata/include_test|testdata/include_test/expected/outs.json"
    "default_out|testdata/default_out|testdata/default_out/expected/outs.json"
    "wildcard|testdata/wildcard|testdata/wildcard/expected/outs.json"
    "multidim|testdata/multidim|testdata/multidim/expected/outs.json"
    "typedmap_out|testdata/typedmap_out|testdata/typedmap_out/expected/outs.json"
    "returnonly|testdata/returnonly|testdata/returnonly/expected/outs.json"
    "multisplit|testdata/multisplit|testdata/multisplit/expected/outs.json"
    "null_in|testdata/null_in|testdata/null_in/expected/outs.json"
    "disabled_callref|testdata/disabled_callref|testdata/disabled_callref/expected/outs.json"
    "struct_input|testdata/struct_input|testdata/struct_input/expected/outs.json"
    "nested_struct|testdata/nested_struct|testdata/nested_struct/expected/outs.json"
    "literals|testdata/literals|testdata/literals/expected/outs.json"
    "fanin|testdata/fanin|testdata/fanin/expected/outs.json"
)

# Run cases in a bounded parallel pool; xargs exits non-zero if any case fails.
printf '%s\n' "${CASES[@]}" |
    xargs -P "${E2E_PARALLEL:-6}" -d '\n' -I{} bash "$0" --one '{}'

# Object-store readiness: a file pipeline under copy-staging into isolated
# scratch dirs, plus self-contained-bundle assertions.
bash "$ROOT/test/e2e/cloud_sim.sh"
