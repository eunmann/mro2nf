#!/usr/bin/env bash
set -euo pipefail

# Object-store readiness check. Runs a file-passing pipeline with Nextflow
# configured to COPY staged inputs into isolated scratch dirs (process.scratch +
# stageInMode/stageOutMode = 'copy') rather than symlinking from a shared work
# tree, and additionally asserts the on-disk shape that makes the bundle model
# portable: each stage's output bundle physically contains its files (under f/)
# and references them by a relative marker, never by a host-absolute path. The
# pre-bundle model (absolute paths in outs.json, files left in the producing
# task's work dir) could not satisfy either property.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

if ! command -v java >/dev/null 2>&1 && [ -s "$HOME/.sdkman/bin/sdkman-init.sh" ]; then
    set +u
    # shellcheck disable=SC1091
    source "$HOME/.sdkman/bin/sdkman-init.sh"
    set -u
fi

# A missing prerequisite skips locally but FAILS under E2E_REQUIRE=1 (set in
# CI), so a runner-image regression cannot turn the check into a silent skip.
missing() {
    [ "${E2E_REQUIRE:-0}" = "1" ] && { echo "FAIL: $1 (required by E2E_REQUIRE=1)"; exit 1; }
    echo "SKIP: $1"
    exit 0
}
command -v nextflow >/dev/null 2>&1 || missing "nextflow not found"
command -v java >/dev/null 2>&1 || missing "java not found"
command -v python3 >/dev/null 2>&1 || missing "python3 not found"

export NXF_ANSI_LOG=false NXF_DISABLE_CHECK_LATEST=true
export NXF_OPTS="${NXF_OPTS:--Xms256m -Xmx1g -XX:+UseSerialGC}"

make build >/dev/null

proj="$(mktemp -d)"
mf="" # the map_file project dir (created later); covered by the same trap
trap 'rm -rf "$proj" ${mf:+"$mf"}' EXIT

./mro2nf -o "$proj" \
    -mre "$ROOT/mre" \
    -shell "$ROOT/vendor-martian/python/martian_shell.py" \
    -mropath testdata/file_chain \
    testdata/file_chain/pipeline.mro >/dev/null

# Force copy-staging into per-task scratch dirs: no input is reachable via a
# shared absolute path, only via Nextflow's staged copy of the channel item.
cat >"$proj/cloud.config" <<'CFG'
process {
    scratch = true
    stageInMode = 'copy'
    stageOutMode = 'copy'
}
CFG

(cd "$proj" && nextflow run main.nf -c cloud.config >/dev/null 2>&1) || { echo "FAIL[cloud_sim]: nextflow"; exit 1; }

# 1. The published result is still correct under copy-staging.
if ! python3 -c "import json,sys; d=json.load(open('$proj/results/pipeline_outs.json')); sys.exit(0 if d=={'y':42.0} else 1)" 2>/dev/null; then
    echo "FAIL[cloud_sim]: result $(cat "$proj/results/pipeline_outs.json"), want {'y': 42.0}"
    exit 1
fi

# 2. A producing stage's output bundle is self-contained: data.json carries a
#    relative file marker (never a host-absolute path), and the file it points to
#    lives inside the bundle.
ok=0
while IFS= read -r data; do
    grep -q '@mre:file:' "$data" || continue
    # The marker must be a relative bundle path, not an absolute one.
    if grep -qE '"@mre:file:/' "$data"; then
        echo "FAIL[cloud_sim]: absolute path in bundle marker ($data)"
        exit 1
    fi
    rel="$(python3 -c "
import json,sys
for v in json.load(open('$data')).values():
    if isinstance(v,str) and v.startswith('@mre:file:'):
        print(v[len('@mre:file:'):]); break
" 2>/dev/null)"
    [ -n "$rel" ] || continue
    if [ ! -e "$(dirname "$data")/$rel" ]; then
        echo "FAIL[cloud_sim]: bundle file $rel missing beside $data"
        exit 1
    fi
    ok=1
    break
done < <(find "$proj/work" -name data.json 2>/dev/null)

if [ "$ok" != "1" ]; then
    echo "FAIL[cloud_sim]: no self-contained file bundle found"
    exit 1
fi

# 3. A map call whose callee emits a FILE must carry per-fork files through the
#    MERGE bundle, not bare absolute paths into deleted fork scratch dirs.
mf="$(mktemp -d)"
./mro2nf -o "$mf" -mre "$ROOT/mre" -shell "$ROOT/vendor-martian/python/martian_shell.py" \
    -mropath testdata/map_file testdata/map_file/pipeline.mro >/dev/null
cp "$proj/cloud.config" "$mf/cloud.config"
if ! (cd "$mf" && nextflow run main.nf -c cloud.config >/dev/null 2>&1); then
    echo "FAIL[cloud_sim]: map_file nextflow"; exit 1
fi
# The txt[] output `fs` publishes as an mrp-style tree: fs/<idx>.txt.
if [ "$(cat "$mf/results/fs/0.txt" 2>/dev/null)" != "val=1" ] ||
    [ "$(cat "$mf/results/fs/1.txt" 2>/dev/null)" != "val=2" ]; then
    echo "FAIL[cloud_sim]: map-call file outputs not staged through merge"
    exit 1
fi

echo "OK[cloud_sim]: copy-staged file + map-call-file pipelines correct, bundles self-contained"
