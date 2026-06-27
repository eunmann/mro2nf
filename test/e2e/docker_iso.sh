#!/usr/bin/env bash
set -euo pipefail

# Container-isolation e2e: run transpiled pipelines under the Nextflow `docker`
# executor, where each task runs in a container that mounts ONLY its work dir —
# not the project directory. This reproduces the AWS Batch + S3 / HealthOmics
# model (isolated workers, no shared filesystem) that the plain local-executor
# suite (run.sh) and the copy-staging proxy (cloud_sim.sh) cannot: a task can
# read a file only if it was staged in as a declared `path` input.
#
# It is the regression guard for the _assets staging fix — if types.json or a
# bindspec is ever referenced via ${projectDir} in a script again, these runs
# fail (the file is invisible inside the container) while run.sh still passes.
#
# Skips cleanly when docker, nextflow, or java is unavailable.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

command -v docker >/dev/null 2>&1 || { echo "SKIP: docker not found"; exit 0; }
docker info >/dev/null 2>&1 || { echo "SKIP: docker not usable"; exit 0; }
command -v nextflow >/dev/null 2>&1 || { echo "SKIP: nextflow not found"; exit 0; }

if ! command -v java >/dev/null 2>&1 && [ -s "$HOME/.sdkman/bin/sdkman-init.sh" ]; then
    set +u
    # shellcheck disable=SC1091
    source "$HOME/.sdkman/bin/sdkman-init.sh"
    set -u
fi
command -v java >/dev/null 2>&1 || { echo "SKIP: java not found"; exit 0; }

export NXF_ANSI_LOG=false
export NXF_DISABLE_CHECK_LATEST=true
export NXF_OPTS="${NXF_OPTS:--Xms256m -Xmx1g -XX:+UseSerialGC}"

IMAGE=mart-iso:test
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# An image that bakes mre, the Martian adapters, and the fixture stage code at
# the SAME absolute paths the transpiler bakes into the generated scripts, so
# they resolve inside the isolated task container. types.json and the bindspecs
# are deliberately NOT baked — they must arrive via the staged _assets input.
make build >/dev/null
docker build -q -t "$IMAGE" -f - "$ROOT" >/dev/null <<DOCKERFILE
FROM python:3.12-slim
RUN apt-get update && apt-get install -y --no-install-recommends procps coreutils && rm -rf /var/lib/apt/lists/*
COPY mre $ROOT/mre
COPY vendor-martian/python $ROOT/vendor-martian/python
COPY testdata $ROOT/testdata
RUN chmod +x $ROOT/mre
DOCKERFILE

# A representative slice across process kinds: single-stage, split, map call
# (fork/merge + keyed), disabled call (null-bundle staging), split-returned join
# override, and the `special` scheduler key.
CASES=(
    "split_test|testdata/split_test|testdata/split_test/expected/outs.json"
    "map_pipe|testdata/map_pipe|testdata/map_pipe/expected/outs.json"
    "disabled_map|testdata/disabled_map|testdata/disabled_map/expected/outs.json"
    "join_resources|testdata/join_resources|testdata/join_resources/expected/outs.json"
    "special_resource|testdata/special_resource|testdata/special_resource/expected/outs.json"
    "map_pipe_nested|testdata/map_pipe_nested|testdata/map_pipe_nested/expected/outs.json"
    "kitchen_sink|testdata/kitchen_sink|testdata/kitchen_sink/expected/main_outs.json"
    "entry_file|testdata/entry_file|testdata/entry_file/expected/ep_outs.json"
    "entry_filearr|testdata/entry_filearr|testdata/entry_filearr/expected/epa_outs.json"
    "entry_struct_file|testdata/entry_struct_file|testdata/entry_struct_file/expected/eps_outs.json"
    "entry_mapfile|testdata/entry_mapfile|testdata/entry_mapfile/expected/epm_outs.json"
    "split_from_file|testdata/split_from_file|testdata/split_from_file/expected/sp_outs.json"
    "map_split_file|testdata/map_split_file|testdata/map_split_file/expected/outs.json"
)

# split_test's golden is a stage _outs; everything else is a plain outs.json.
declare -A GOLD=(
    [split_test]="testdata/split_test/expected/SUM_SQUARE_PIPELINE/fork0/_outs"
)

fail=0
for spec in "${CASES[@]}"; do
    IFS='|' read -r name dir golden <<<"$spec"
    [ -n "${GOLD[$name]:-}" ] && golden="${GOLD[$name]}"
    proj="$WORK/$name"

    ./mart -o "$proj" -mre "$ROOT/mre" -shell "$ROOT/vendor-martian/python/martian_shell.py" \
        -mropath "$dir" "$dir/pipeline.mro" >/dev/null 2>&1 ||
        { echo "FAIL[$name]: transpile"; fail=1; continue; }

    ( cd "$proj" && nextflow run main.nf -with-docker "$IMAGE" >run.log 2>&1 ) ||
        { echo "FAIL[$name]: nextflow (isolated)"; tail -4 "$proj/run.log" | sed 's/^/    /'; fail=1; continue; }

    python3 - "$proj/results/pipeline_outs.json" "$golden" "$name" <<'PY' || fail=1
import json, sys
got = json.load(open(sys.argv[1]))
gold = json.load(open(sys.argv[2]))
ok = got == gold
print(("ISO-OK" if ok else "ISO-FAIL") + f"[{sys.argv[3]}]: {json.dumps(got, sort_keys=True)}")
sys.exit(0 if ok else 1)
PY
done

# File-typed entry-input overrides under isolation: the strictest staging proof.
# Every override file lives in $WORK (NOT baked into the image), so the stage can
# only read its content if Nextflow staged file(params.<name>) into the container
# — exactly the AWS Batch / HealthOmics S3-localization path. Covers a scalar file,
# a file[] (list), and a struct-with-file, each reconstructed by mre entryargs.
printf '5\n7\n9\n'  >"$WORK/o_scalar.txt"   # 21 * 2 == 42
printf '2\n3\n'     >"$WORK/o_arr1.txt"     # 5
printf '10\n'       >"$WORK/o_arr2.txt"     # 10 ; (5+10) * 2 == 30
printf '4\n4\n'     >"$WORK/o_ref.txt"      # 8  ; * n(5) == 40
printf '6\n'        >"$WORK/o_m1.txt"       # 6
printf '14\n'       >"$WORK/o_m2.txt"       # 14 ; (6+14) * 2 == 40

run_override() { # name fixture params-json expected-json
    local name="$1" fx="$2" params="$3" expect="$4"
    local proj="$WORK/$name"
    if ! ./mart -o "$proj" -mre "$ROOT/mre" -shell "$ROOT/vendor-martian/python/martian_shell.py" \
        -mropath "testdata/$fx" "testdata/$fx/pipeline.mro" >/dev/null 2>&1; then
        echo "FAIL[$name]: transpile"; fail=1; return
    fi
    printf '%s' "$params" >"$proj/params.json"
    if ! ( cd "$proj" && nextflow run main.nf -params-file params.json -with-docker "$IMAGE" >run.log 2>&1 ); then
        echo "FAIL[$name]: nextflow (isolated)"; tail -4 "$proj/run.log" | sed 's/^/    /'; fail=1; return
    fi
    EXPECT="$expect" python3 - "$proj/results/pipeline_outs.json" "$name" <<'PY' || fail=1
import json, os, sys
got = json.load(open(sys.argv[1]))
ok = got == json.loads(os.environ["EXPECT"])
print(("ISO-OK" if ok else "ISO-FAIL") + f"[{sys.argv[2]}]: {json.dumps(got, sort_keys=True)}")
sys.exit(0 if ok else 1)
PY
}

run_override entry_file_override        entry_file        "{\"reads\": \"$WORK/o_scalar.txt\"}"                          '{"total": 42.0}'
run_override entry_filearr_override      entry_filearr     "{\"reads\": [\"$WORK/o_arr1.txt\", \"$WORK/o_arr2.txt\"]}"    '{"total": 30.0}'
run_override entry_struct_file_override  entry_struct_file "{\"cfg\": {\"ref\": \"$WORK/o_ref.txt\", \"n\": 5}}"          '{"total": 40.0}'
run_override entry_mapfile_override       entry_mapfile     "{\"reads\": {\"a\": \"$WORK/o_m1.txt\", \"b\": \"$WORK/o_m2.txt\"}}" '{"total": 40.0}'

exit "$fail"
