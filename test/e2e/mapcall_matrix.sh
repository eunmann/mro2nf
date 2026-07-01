#!/usr/bin/env bash
set -u

# Differential exploration of map-call source variations: for each shape of the
# collection a `map call` iterates, run the pipeline through REAL Martian (mrp)
# AND the transpiled Nextflow and print the resolved output side by side. This
# is the oracle for conformance finding #4 (empty/zero-fork map calls): Martian's
# result depends on literal-vs-runtime and array-vs-map in ways mro2nf does not
# yet fully reproduce. Informational (exit 0); DIFF rows are the known gaps
# documented in docs/MARTIAN_RUNTIME_CONFORMANCE.md.
#
# Requires a local Martian build: set MARTIAN_BIN (default ~/workdir/martian/bin).

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"
MARTIAN_BIN="${MARTIAN_BIN:-$HOME/workdir/martian/bin}"

if ! command -v java >/dev/null 2>&1 && [ -s "$HOME/.sdkman/bin/sdkman-init.sh" ]; then
    set +u
    # shellcheck disable=SC1091
    source "$HOME/.sdkman/bin/sdkman-init.sh"
    set -u
fi

for tool in nextflow java python3; do
    command -v "$tool" >/dev/null 2>&1 || { echo "SKIP[mapcall_matrix]: $tool not found"; exit 0; }
done
[ -x "$MARTIAN_BIN/mrp" ] || { echo "SKIP[mapcall_matrix]: mrp not found (set MARTIAN_BIN)"; exit 0; }

export NXF_ANSI_LOG=false NXF_DISABLE_CHECK_LATEST=true
export NXF_OPTS="${NXF_OPTS:--Xms256m -Xmx1g -XX:+UseSerialGC}"
make build >/dev/null

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/stages/scale" "$WORK/stages/mkarr" "$WORK/stages/mkmap"
printf 'def main(args, outs):\n    outs.scaled = args.value * 2\n' > "$WORK/stages/scale/__init__.py"
printf 'def main(args, outs):\n    outs.xs = [float(i) for i in range(args.n)]\n' > "$WORK/stages/mkarr/__init__.py"
printf 'def main(args, outs):\n    outs.xs = {("k%%d" %% i): float(i) for i in range(args.n)}\n' > "$WORK/stages/mkmap/__init__.py"

mrp_scaled() {
    local dir="$1" tmp; tmp="$(mktemp -d)"
    ( cd "$tmp" && MROPATH="$dir" "$MARTIAN_BIN/mrp" "$dir/pipeline.mro" mrp \
        --jobmode=local --disable-ui --nopreflight >/dev/null 2>&1 )
    python3 -c "import json,glob
f=glob.glob('$tmp/mrp/*/fork0/_outs')
print(json.dumps(json.load(open(f[0]))['scaled']) if f else 'ERR')" 2>/dev/null || echo ERR
    rm -rf "$tmp"
}

nf_scaled() {
    local dir="$1" proj; proj="$(mktemp -d)"
    ./mro2nf -o "$proj" -mre "$ROOT/mre" -shell "$ROOT/vendor-martian/python/martian_shell.py" \
        -mropath "$dir" "$dir/pipeline.mro" >/dev/null 2>&1
    ( cd "$proj" && nextflow run main.nf >/dev/null 2>&1 )
    python3 -c "import json; print(json.dumps(json.load(open('$proj/results/pipeline_outs.json'))['scaled']))" 2>/dev/null || echo ERR
    rm -rf "$proj"
}

probe() {
    local name="$1" body="$2" dir="$WORK/case"
    rm -rf "$dir"; mkdir -p "$dir"; cp -r "$WORK/stages" "$dir/"
    printf '%s\n' "$body" > "$dir/pipeline.mro"
    local m n; m="$(mrp_scaled "$dir")"; n="$(nf_scaled "$dir")"
    local mark="OK  "; [ "$m" != "$n" ] && mark="DIFF"
    printf '%-20s mrp=%-24s nextflow=%-24s %s\n' "$name" "$m" "$n" "$mark"
}

SCALE='stage SCALE(in float value, out float scaled, src py "stages/scale",)'
MKARR='stage MKARR(in int n, out float[] xs, src py "stages/mkarr",)'
MKMAP='stage MKMAP(in int n, out map<float> xs, src py "stages/mkmap",)'

echo "map-call source matrix (mrp = source of truth):"
probe lit_empty_array "$SCALE
pipeline P(in float[] v, out float[] scaled,){ map call SCALE(value=split self.v,) return(scaled=SCALE.scaled,) }
call P(v=[],)"
probe lit_empty_map "$SCALE
pipeline P(in map<float> v, out map<float> scaled,){ map call SCALE(value=split self.v,) return(scaled=SCALE.scaled,) }
call P(v={},)"
probe lit_nonempty_array "$SCALE
pipeline P(in float[] v, out float[] scaled,){ map call SCALE(value=split self.v,) return(scaled=SCALE.scaled,) }
call P(v=[1.0,2.0],)"
probe lit_nonempty_map "$SCALE
pipeline P(in map<float> v, out map<float> scaled,){ map call SCALE(value=split self.v,) return(scaled=SCALE.scaled,) }
call P(v={\"a\":1.0,\"b\":2.0},)"
probe rt_empty_array "$SCALE
$MKARR
pipeline P(in int n, out float[] scaled,){ call MKARR(n=self.n,) map call SCALE(value=split MKARR.xs,) return(scaled=SCALE.scaled,) }
call P(n=0,)"
probe rt_empty_map "$SCALE
$MKMAP
pipeline P(in int n, out map<float> scaled,){ call MKMAP(n=self.n,) map call SCALE(value=split MKMAP.xs,) return(scaled=SCALE.scaled,) }
call P(n=0,)"
probe rt_nonempty_array "$SCALE
$MKARR
pipeline P(in int n, out float[] scaled,){ call MKARR(n=self.n,) map call SCALE(value=split MKARR.xs,) return(scaled=SCALE.scaled,) }
call P(n=2,)"
probe rt_nonempty_map "$SCALE
$MKMAP
pipeline P(in int n, out map<float> scaled,){ call MKMAP(n=self.n,) map call SCALE(value=split MKMAP.xs,) return(scaled=SCALE.scaled,) }
call P(n=2,)"
