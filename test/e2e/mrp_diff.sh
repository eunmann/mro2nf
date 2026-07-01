#!/usr/bin/env bash
set -euo pipefail

# True differential test: run each fixture through REAL Martian (mrp) AND the
# transpiled Nextflow, then compare the published outs/ tree — the set of output
# file paths relative to the outs dir plus their contents — and the (path-
# normalized) _outs JSON. This validates mre's publish layout and the whole
# transpile+runtime path against Martian itself, not a hand-written golden.
#
# Requires a local Martian build: set MARTIAN_BIN (default ~/workdir/martian/bin).
# Skips cleanly when mrp, nextflow, java, or python3 is unavailable, so CI without
# a Martian checkout is unaffected. Run a single case with:
#   bash test/e2e/mrp_diff.sh <fixture-name>

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"
MARTIAN_BIN="${MARTIAN_BIN:-$HOME/workdir/martian/bin}"

if ! command -v java >/dev/null 2>&1 && [ -s "$HOME/.sdkman/bin/sdkman-init.sh" ]; then
    set +u
    # shellcheck disable=SC1091
    source "$HOME/.sdkman/bin/sdkman-init.sh"
    set -u
fi

command -v nextflow >/dev/null 2>&1 || { echo "SKIP[mrp_diff]: nextflow not found"; exit 0; }
command -v java >/dev/null 2>&1 || { echo "SKIP[mrp_diff]: java not found"; exit 0; }
command -v python3 >/dev/null 2>&1 || { echo "SKIP[mrp_diff]: python3 not found"; exit 0; }
[ -x "$MARTIAN_BIN/mrp" ] || { echo "SKIP[mrp_diff]: mrp not found (set MARTIAN_BIN=<martian>/bin)"; exit 0; }

export NXF_ANSI_LOG=false NXF_DISABLE_CHECK_LATEST=true
export NXF_OPTS="${NXF_OPTS:--Xms256m -Xmx1g -XX:+UseSerialGC}"

make build >/dev/null

# Fixtures whose top-level `call` runs standalone (no launch-time param override).
# Each exercises a distinct output/pipeline shape.
CASES=(
    file_min dir_out
    map_file map_file_keyed map_split_file
    struct_of_file struct_file_array
    diamond_min fork_min struct_min kitchen_sink
    file_tree
)

# tree DIR EXCLUDE -> "relpath  sha256" for every regular file, sorted. An
# absent dir (a pipeline with no file outputs) yields an empty tree.
tree() {
    [ -d "$1" ] || return 0
    ( cd "$1" && find . -type f ! -name "$2" -printf '%P\n' | LC_ALL=C sort |
        while IFS= read -r f; do printf '%s  %s\n' "$f" "$(sha256sum "$f" | cut -d' ' -f1)"; done )
}

run_one() {
    local name="$1" dir="testdata/$1"
    [ -f "$dir/pipeline.mro" ] || { echo "SKIP[$name]: no fixture"; return 0; }

    local tmp; tmp="$(mktemp -d)"
    # shellcheck disable=SC2064
    trap "rm -rf '$tmp'" RETURN

    # --- Martian --- mrp requires a plain pipestance name and creates it in the
    # cwd, so run from a temp dir. Symlink the fixture's input/ there so relative
    # input paths in the .mro (e.g. "input/x.txt") resolve as they do for the
    # transpiler's -mropath.
    [ -d "$ROOT/$dir/input" ] && ln -s "$ROOT/$dir/input" "$tmp/input"
    if ! ( cd "$tmp" && MROPATH="$ROOT/$dir" "$MARTIAN_BIN/mrp" "$ROOT/$dir/pipeline.mro" mrp \
            --jobmode=local --localcores=2 --localmem=4 --disable-ui --nopreflight >"$tmp/mrp.log" 2>&1 ); then
        echo "FAIL[$name]: mrp"; grep -iE "error|assert|traceback|no such|invalid" "$tmp/mrp.log" | head -5; return 1
    fi

    # --- Nextflow ---
    local proj="$tmp/nf"; mkdir -p "$proj"
    local mrjob_opt=(); [ -f "$dir/mrjob.sh" ] && mrjob_opt=(-mrjob "$dir/mrjob.sh")
    if ! ./mro2nf -o "$proj" -mre "$ROOT/mre" -shell "$ROOT/vendor-martian/python/martian_shell.py" \
            -mropath "$dir" ${mrjob_opt[@]+"${mrjob_opt[@]}"} "$dir/pipeline.mro" >/dev/null 2>&1; then
        echo "FAIL[$name]: transpile"; return 1
    fi
    if ! ( cd "$proj" && nextflow run main.nf >nf.log 2>&1 ); then
        echo "FAIL[$name]: nextflow"; sed 's/^/    /' "$proj/nf.log" | tail -5; return 1
    fi

    # --- compare the published file tree (paths + contents) ---
    local mrp_outs="$tmp/mrp/outs" nf_outs="$proj/results"
    local a b; a="$(tree "$mrp_outs" _outs)"; b="$(tree "$nf_outs" pipeline_outs.json)"
    if [ "$a" != "$b" ]; then
        echo "FAIL[$name]: published tree differs (mrp vs nextflow)"
        diff <(printf '%s\n' "$a") <(printf '%s\n' "$b") | sed 's/^/    /' | head -20
        return 1
    fi

    # --- compare the _outs JSON (mrp abs paths normalized to relative) ---
    # Martian rewrites the published outs into the TOP pipeline's fork0 metadata
    # (<pipestance>/<PIPELINE>/fork0/_outs); outs/ holds only the file tree.
    local mrp_json; mrp_json="$(ls -d "$tmp"/mrp/*/fork0/_outs 2>/dev/null | LC_ALL=C sort | head -1)"
    if [ -z "$mrp_json" ]; then echo "FAIL[$name]: no mrp fork0 _outs"; return 1; fi
    local out
    if ! out="$(python3 "$ROOT/test/e2e/normcmp.py" "$mrp_json" "$mrp_outs" "$nf_outs/pipeline_outs.json" "$name")"; then
        echo "$out"; return 1
    fi

    echo "OK[$name]: outs tree + json match Martian ($(printf '%s' "$a" | grep -c . ) file(s))"
}

if [ $# -ge 1 ]; then
    run_one "$1"; exit $?
fi

rc=0
for c in "${CASES[@]}"; do run_one "$c" || rc=1; done
exit $rc
