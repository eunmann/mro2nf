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

# A missing prerequisite skips locally but FAILS under E2E_REQUIRE=1 (set in
# CI), so a runner-image regression cannot turn the whole suite into a silent
# green skip.
missing() {
    [ "${E2E_REQUIRE:-0}" = "1" ] && { echo "FAIL: $1 (required by E2E_REQUIRE=1)"; exit 1; }
    echo "SKIP: $1"
    exit 0
}
command -v docker >/dev/null 2>&1 || missing "docker not found"
docker info >/dev/null 2>&1 || missing "docker not usable"
command -v nextflow >/dev/null 2>&1 || missing "nextflow not found"

if ! command -v java >/dev/null 2>&1 && [ -s "$HOME/.sdkman/bin/sdkman-init.sh" ]; then
    set +u
    # shellcheck disable=SC1091
    source "$HOME/.sdkman/bin/sdkman-init.sh"
    set -u
fi
command -v java >/dev/null 2>&1 || missing "java not found"

export NXF_ANSI_LOG=false
export NXF_DISABLE_CHECK_LATEST=true
export NXF_OPTS="${NXF_OPTS:--Xms256m -Xmx1g -XX:+UseSerialGC}"

IMAGE=mro2nf-iso:test
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# On rootful Docker (GitHub Actions) containers default to root, so task outputs
# come back root-owned: the head-node Groovy that reads a chunk's data.json can be
# denied, and the work-dir cleanup fails with "Permission denied". Mapping the
# container to the runner's uid/gid (with a writable HOME) keeps files owned by
# the runner. But on ROOTLESS Docker the opposite holds — the container's root
# already maps to the host user, and forcing -u maps to an unprivileged subuid
# that cannot write the bind-mounted work dir — so only apply -u when rootful.
ISO_CFG="$WORK/iso.config"
if docker info -f '{{.SecurityOptions}}' 2>/dev/null | grep -q rootless; then
    : >"$ISO_CFG" # rootless: default (container root -> host user) is correct
else
    printf "docker.runOptions = '-u %s:%s -e HOME=/tmp'\n" "$(id -u)" "$(id -g)" >"$ISO_CFG"
fi

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
    "split_test|testdata/split_test|testdata/split_test/expected/SUM_SQUARE_PIPELINE/fork0/_outs"
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
    "struct_file_array|testdata/struct_file_array|testdata/struct_file_array/expected/outs.json"
    # Adapter coverage under isolation: exec and comp (comp needs -mrjob; the
    # fixture's mrjob.sh is baked into the image with testdata/).
    "exec_min|testdata/exec_min|testdata/exec_min/expected/ep_outs.json"
    "mixed_adapters|testdata/mixed_adapters|testdata/mixed_adapters/expected/outs.json"
    # Null-bundle / zero-chunk staging — the shapes most likely to break on
    # isolated workers.
    "empty_fork_min|testdata/empty_fork_min|testdata/empty_fork_min/expected/outs.json"
    "map_null_map|testdata/map_null_map|testdata/map_null_map/expected/outs.json"
    "disabled_callref|testdata/disabled_callref|testdata/disabled_callref/expected/outs.json"
)

fail=0
for spec in "${CASES[@]}"; do
    IFS='|' read -r name dir golden <<<"$spec"
    proj="$WORK/$name"

    # A comp-adapter fixture ships its mrjob wrapper alongside the .mro; the
    # baked path resolves inside the image because testdata/ is copied in.
    mrjob_opt=()
    [ -f "$dir/mrjob.sh" ] && mrjob_opt=(-mrjob "$ROOT/$dir/mrjob.sh")

    ./mro2nf -o "$proj" -mre "$ROOT/mre" -shell "$ROOT/vendor-martian/python/martian_shell.py" \
        -mropath "$dir" ${mrjob_opt[@]+"${mrjob_opt[@]}"} "$dir/pipeline.mro" >/dev/null 2>&1 ||
        { echo "FAIL[$name]: transpile"; fail=1; continue; }

    ( cd "$proj" && nextflow run main.nf -c "$ISO_CFG" -with-docker "$IMAGE" >run.log 2>&1 ) ||
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

# Same-basename file[] leaves (sb1/reads.txt + sb2/reads.txt): the regression
# guard for the entry-file staging collision. Both leaves share the basename
# reads.txt; only the per-index `stageAs: '<in>_?/*'` subdir keeps them from
# clobbering each other in the isolated task work dir. Without it one file
# overwrites the other and the total is wrong.
mkdir -p "$WORK/sb1" "$WORK/sb2"
printf '2\n3\n'     >"$WORK/sb1/reads.txt"  # 5
printf '10\n'       >"$WORK/sb2/reads.txt"  # 10 ; (5+10) * 2 == 30

run_override() { # name fixture params-json expected-json
    local name="$1" fx="$2" params="$3" expect="$4"
    local proj="$WORK/$name"
    if ! ./mro2nf -o "$proj" -mre "$ROOT/mre" -shell "$ROOT/vendor-martian/python/martian_shell.py" \
        -mropath "testdata/$fx" "testdata/$fx/pipeline.mro" >/dev/null 2>&1; then
        echo "FAIL[$name]: transpile"; fail=1; return
    fi
    printf '%s' "$params" >"$proj/params.json"
    if ! ( cd "$proj" && nextflow run main.nf -c "$ISO_CFG" -params-file params.json -with-docker "$IMAGE" >run.log 2>&1 ); then
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

# --- the GENERATED cloud artifacts, validated without a live account ---------
# 1. -target awsbatch: build the runtime image from the EMITTED Dockerfile
#    (not this harness's inline one) and run the pipeline with only that
#    image's baked /opt/mro2nf runtime, executor overridden to local+docker.
GEN_IMAGE=mro2nf-gen:test
gen="$WORK/gen_awsbatch"
if ! ./mro2nf -o "$gen" -target awsbatch -container "$GEN_IMAGE" \
    -mre "$ROOT/mre" -shell "$ROOT/vendor-martian/python/martian_shell.py" \
    -mropath testdata/diamond_min testdata/diamond_min/pipeline.mro >/dev/null 2>&1; then
    echo "FAIL[gen_awsbatch]: transpile"; fail=1
elif ! docker build -q -t "$GEN_IMAGE" "$gen" >/dev/null 2>&1; then
    echo "FAIL[gen_awsbatch]: docker build of the generated Dockerfile"; fail=1
else
    { echo "process.executor = 'local'"; echo "docker.enabled = true"; cat "$ISO_CFG"; } >"$gen/local.config"
    if ! ( cd "$gen" && nextflow run main.nf -c local.config --aws_outdir "$gen/results" >run.log 2>&1 ); then
        echo "FAIL[gen_awsbatch]: nextflow (generated image)"; tail -4 "$gen/run.log" | sed 's/^/    /'; fail=1
    else
        python3 - "$gen/results/pipeline_outs.json" testdata/diamond_min/expected/outs.json gen_awsbatch <<'PY' || fail=1
import json, sys
got, gold = json.load(open(sys.argv[1])), json.load(open(sys.argv[2]))
ok = got == gold
print(("ISO-OK" if ok else "ISO-FAIL") + f"[{sys.argv[3]}]: {json.dumps(got, sort_keys=True)}")
sys.exit(0 if ok else 1)
PY
    fi
fi

# 2. -target healthomics: the packaging artifacts must be well-formed —
#    package.sh builds a zip containing the workflow (not the build context),
#    and parameter-template.json parses with the container + entry params.
if command -v zip >/dev/null 2>&1; then
    omics="$WORK/gen_omics"
    if ! ./mro2nf -o "$omics" -target healthomics -container "$GEN_IMAGE" \
        -mre "$ROOT/mre" -shell "$ROOT/vendor-martian/python/martian_shell.py" \
        -mropath testdata/entry_file testdata/entry_file/pipeline.mro >/dev/null 2>&1; then
        echo "FAIL[gen_omics]: transpile"; fail=1
    elif ! ( cd "$omics" && bash package.sh >/dev/null 2>&1 ); then
        echo "FAIL[gen_omics]: package.sh"; fail=1
    elif ! ( cd "$omics" && unzip -l workflow.zip | grep -q ' main.nf$' ); then
        echo "FAIL[gen_omics]: workflow.zip missing main.nf"; fail=1
    elif ( cd "$omics" && unzip -l workflow.zip | grep -q 'runtime/mre' ); then
        echo "FAIL[gen_omics]: workflow.zip must exclude the docker build context"; fail=1
    elif ! python3 -c "
import json, sys
t = json.load(open('$omics/parameter-template.json'))
sys.exit(0 if 'container' in t and 'reads' in t and t['reads'].get('optional') else 1)
"; then
        echo "FAIL[gen_omics]: parameter-template.json missing container/reads entries"; fail=1
    else
        echo "ISO-OK[gen_omics]: package.sh zip + parameter template well-formed"
    fi
else
    echo "SKIP[gen_omics]: zip not found"
fi

run_override entry_file_override        entry_file        "{\"reads\": \"$WORK/o_scalar.txt\"}"                          '{"total": 42.0}'
run_override entry_filearr_override      entry_filearr     "{\"reads\": [\"$WORK/o_arr1.txt\", \"$WORK/o_arr2.txt\"]}"    '{"total": 30.0}'
run_override entry_filearr_samebasename  entry_filearr     "{\"reads\": [\"$WORK/sb1/reads.txt\", \"$WORK/sb2/reads.txt\"]}" '{"total": 30.0}'
run_override entry_struct_file_override  entry_struct_file "{\"cfg\": {\"ref\": \"$WORK/o_ref.txt\", \"n\": 5}}"          '{"total": 40.0}'
run_override entry_mapfile_override       entry_mapfile     "{\"reads\": {\"a\": \"$WORK/o_m1.txt\", \"b\": \"$WORK/o_m2.txt\"}}" '{"total": 40.0}'

exit "$fail"
