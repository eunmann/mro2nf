#!/usr/bin/env bash
set -euo pipefail

# Launch-time runtime knobs no golden-diff case exercises:
#
#   1. -resume determinism: rerunning an unchanged, successful pipeline with
#      -resume must execute ZERO new tasks (everything cached). A cache-key
#      instability (e.g. a timestamp leaking into a staged asset) would
#      otherwise ship silently and destroy resumability on long runs.
#   2. `mro2nf overrides`: the converted -c overlay must actually retune the
#      targeted stage — verified from the _jobinfo the stage saw, not from the
#      generated config text.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

if ! command -v java >/dev/null 2>&1 && [ -s "$HOME/.sdkman/bin/sdkman-init.sh" ]; then
    set +u
    # shellcheck disable=SC1091
    source "$HOME/.sdkman/bin/sdkman-init.sh"
    set -u
fi

missing() {
    [ "${E2E_REQUIRE:-0}" = "1" ] && { echo "FAIL[runtime_knobs]: $1 (required by E2E_REQUIRE=1)"; exit 1; }
    echo "SKIP[runtime_knobs]: $1"
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
fail=0

proj="$WORK/diamond"
./mro2nf -o "$proj" -mre "$ROOT/mre" \
    -shell "$ROOT/vendor-martian/python/martian_shell.py" \
    -mropath testdata/diamond_min testdata/diamond_min/pipeline.mro >/dev/null

# --- 1. -resume: second run all cached ---------------------------------------
(cd "$proj" && nextflow run main.nf >run1.log 2>&1) || { echo "FAIL[resume]: first run"; exit 1; }
if ! (cd "$proj" && nextflow run main.nf -resume -with-trace trace2.txt >run2.log 2>&1); then
    echo "FAIL[resume]: -resume rerun failed"; tail -5 "$proj/run2.log" | sed 's/^/    /'; fail=1
else
    # Every trace row of the resumed run must be CACHED; any COMPLETED row is a
    # task that re-executed, i.e. an unstable cache key.
    fresh="$(awk -F'\t' 'NR > 1 && $5 != "CACHED" { c++ } END { print c + 0 }' "$proj/trace2.txt")"
    total="$(awk 'END { print NR - 1 }' "$proj/trace2.txt")"
    if [ "$fresh" != "0" ] || [ "$total" = "0" ]; then
        echo "FAIL[resume]: $fresh of $total tasks re-executed under -resume (want 0 of >0)"
        sed 's/^/    /' "$proj/trace2.txt"
        fail=1
    else
        echo "OK[resume]: all $total tasks cached on -resume"
    fi
fi

# --- 2. mro2nf overrides overlay reaches the stage ---------------------------
cat >"$WORK/overrides.json" <<'JSON'
{ "D.GEN": { "mem_gb": 3, "threads": 2 } }
JSON
./mro2nf overrides "$WORK/overrides.json" >"$WORK/overrides.config" 2>/dev/null

proj2="$WORK/diamond_ov"
./mro2nf -o "$proj2" -mre "$ROOT/mre" \
    -shell "$ROOT/vendor-martian/python/martian_shell.py" \
    -mropath testdata/diamond_min testdata/diamond_min/pipeline.mro >/dev/null

if ! (cd "$proj2" && nextflow run main.nf -c "$WORK/overrides.config" >run.log 2>&1); then
    echo "FAIL[overrides]: run with -c overlay failed"; tail -5 "$proj2/run.log" | sed 's/^/    /'; fail=1
else
    # The GEN stage's _jobinfo must report the retuned allocation; ADD (not
    # targeted) must keep its default. _jobinfo's `name` carries the entry
    # callable, so identify the stage from the task dir's .command.run header.
    if python3 - "$proj2/work" <<'PY'; then
import json, pathlib, sys
gen = add = None
for p in pathlib.Path(sys.argv[1]).rglob("_jobinfo"):
    run = p.parent.parent / ".command.run"
    if not run.exists():
        continue
    head = run.read_text(errors="replace").splitlines()[:3]
    task = " ".join(head)
    info = json.load(open(p))
    if "__GEN" in task:
        gen = info
    elif "__ADD" in task:
        add = info
ok = (
    gen is not None and gen["memGB"] == 3 and gen["threads"] == 2
    and add is not None and add["memGB"] != 3
)
sys.exit(0 if ok else 1)
PY
        echo "OK[overrides]: GEN retuned to mem 3 GB / 2 cpus; ADD untouched"
    else
        echo "FAIL[overrides]: retuned allocation did not reach the stage's _jobinfo"
        fail=1
    fi
fi

exit "$fail"
