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

command -v nextflow >/dev/null 2>&1 || { echo "SKIP: nextflow not found"; exit 0; }
command -v python3 >/dev/null 2>&1 || { echo "SKIP: python3 not found"; exit 0; }

export NXF_ANSI_LOG=false NXF_DISABLE_CHECK_LATEST=true
export NXF_OPTS="${NXF_OPTS:--Xms256m -Xmx1g -XX:+UseSerialGC}"

make build >/dev/null

proj="$(mktemp -d)"
trap 'rm -rf "$proj"' EXIT

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
    echo "FAIL[cloud_sim]: map_file nextflow"; rm -rf "$mf"; exit 1
fi
# The txt[] output `fs` publishes as an mrp-style tree: fs/<idx>.txt.
if [ "$(cat "$mf/results/fs/0.txt" 2>/dev/null)" != "val=1" ] ||
    [ "$(cat "$mf/results/fs/1.txt" 2>/dev/null)" != "val=2" ]; then
    echo "FAIL[cloud_sim]: map-call file outputs not staged through merge"
    rm -rf "$mf"; exit 1
fi
rm -rf "$mf"

# 4. A split stage that creates a file into its chunk def (an `in file` chunk
#    param) and reads it in the JOIN must carry that split-produced file to the
#    join, not a bare absolute path into the split's scratch. Mirrors CellRanger
#    MAKE_SHARD (feature_reference). Without staging the chunk-def bundles to the
#    join, the join fails to open the file under copy-staging.
sdf="$(mktemp -d)"
./mro2nf -o "$sdf" -mre "$ROOT/mre" -shell "$ROOT/vendor-martian/python/martian_shell.py" \
    -mropath testdata/split_def_file testdata/split_def_file/pipeline.mro >/dev/null
cp "$proj/cloud.config" "$sdf/cloud.config"
if ! (cd "$sdf" && nextflow run main.nf -c cloud.config >/dev/null 2>&1); then
    echo "FAIL[cloud_sim]: split_def_file nextflow (split-produced def file not staged to join?)"; rm -rf "$sdf"; exit 1
fi
if ! python3 -c "import json,sys; d=json.load(open('$sdf/results/pipeline_outs.json')); sys.exit(0 if d=={'total':403} else 1)" 2>/dev/null; then
    echo "FAIL[cloud_sim]: split_def_file result $(cat "$sdf/results/pipeline_outs.json" 2>/dev/null), want {'total': 403}"
    rm -rf "$sdf"; exit 1
fi
rm -rf "$sdf"

# 5. A sub-pipeline's whole output bundle, forwarded by callable-name type
#    (`bundle = SUB` into `in SUB bundle`) and projected in a consumer, must carry
#    its file leaves through the bundle. Mirrors CellRanger `matrix_computer_outs
#    _SLFE_MATRIX_COMPUTER` forwarded into POST_MATRIX_COMPUTATION, whose
#    FILTER_BARCODES reads a file field. The callee name is not a declared struct,
#    so without registering each callable's outputs as a walk struct the files stay
#    task-local paths and the consumer fails to open them under copy-staging.
pof="$(mktemp -d)"
./mro2nf -o "$pof" -mre "$ROOT/mre" -shell "$ROOT/vendor-martian/python/martian_shell.py" \
    -mropath testdata/pipe_outs_forward testdata/pipe_outs_forward/pipeline.mro >/dev/null
cp "$proj/cloud.config" "$pof/cloud.config"
if ! (cd "$pof" && nextflow run main.nf -c cloud.config >/dev/null 2>&1); then
    echo "FAIL[cloud_sim]: pipe_outs_forward nextflow (callable-typed bundle files not staged?)"; rm -rf "$pof"; exit 1
fi
if ! python3 -c "import json,sys; d=json.load(open('$pof/results/pipeline_outs.json')); sys.exit(0 if d=={'total':43} else 1)" 2>/dev/null; then
    echo "FAIL[cloud_sim]: pipe_outs_forward result $(cat "$pof/results/pipeline_outs.json" 2>/dev/null), want {'total': 43}"
    rm -rf "$pof"; exit 1
fi
rm -rf "$pof"

echo "OK[cloud_sim]: copy-staged file + map-call-file + split-def-file + pipe-outs-forward pipelines correct, bundles self-contained"
