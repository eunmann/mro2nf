#!/usr/bin/env python3
"""Compare a Martian _outs against a transpiled pipeline_outs.json.

Martian writes absolute paths into the pipestance outs/ dir; mro2nf writes the
same paths relative to its results/ dir. Normalize Martian's values by stripping
the outs-dir prefix, then compare structurally.

Usage: normcmp.py <mrp_outs_json> <mrp_outs_dir> <nf_outs_json> <name>
"""
import json
import sys


def main():
    mrp_json, mrp_dir, nf_json, name = sys.argv[1:5]
    prefix = mrp_dir.rstrip("/") + "/"

    def norm(v):
        if isinstance(v, str):
            return v[len(prefix):] if v.startswith(prefix) else v
        if isinstance(v, list):
            return [norm(x) for x in v]
        if isinstance(v, dict):
            return {k: norm(x) for k, x in v.items()}
        return v

    with open(mrp_json) as f:
        mrp = norm(json.load(f))
    with open(nf_json) as f:
        nf = json.load(f)

    if mrp == nf:
        print(f"  json: match")
        return 0

    print(f"FAIL[{name}]: _outs json mismatch")
    print(f"  mrp(normalized) = {json.dumps(mrp, sort_keys=True)}")
    print(f"  nextflow        = {json.dumps(nf, sort_keys=True)}")
    return 1


if __name__ == "__main__":
    sys.exit(main())
