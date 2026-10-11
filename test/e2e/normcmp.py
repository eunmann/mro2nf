#!/usr/bin/env python3
"""Compare a Martian _outs against a transpiled pipeline_outs.json.

Objects compare with their key order and numbers by their source token. A
normalization that excuses a real difference applies only while the divergence
allowlist (divergences.json beside this script, or the fifth argument) has an
entry for its comparator rule, as in the Go differential (published_test.go):

- outs-json-strip-outs-prefix: mrp writes absolute paths into the pipestance
  outs/ dir, mro2nf writes the same paths relative to its results/ dir, so the
  outs-dir prefix is stripped from mrp's strings.
- outs-json-sort-object-keys: object keys are sorted on both sides (#366).

Usage: normcmp.py <mrp_outs_json> <mrp_outs_dir> <nf_outs_json> <name> [<allowlist>]
"""
import json
import os
import sys

STRIP_OUTS_PREFIX = "outs-json-strip-outs-prefix"
SORT_OBJECT_KEYS = "outs-json-sort-object-keys"

OBJECT = "<object>"
NUMBER = "<number>"


def number(token):
    return (NUMBER, token)


def load(path):
    with open(path) as f:
        return json.load(
            f,
            object_pairs_hook=lambda pairs: (OBJECT, pairs),
            parse_float=number,
            parse_int=number,
            parse_constant=number,
        )


def excused_rules(path):
    with open(path) as f:
        doc = json.load(f)
    return {e["comparator"]["rule"] for e in doc["divergences"] if "comparator" in e}


def norm(v, prefix, sort_keys):
    if isinstance(v, str):
        return v[len(prefix):] if prefix and v.startswith(prefix) else v
    if isinstance(v, list):
        return [norm(x, prefix, sort_keys) for x in v]
    if isinstance(v, tuple) and v[0] == OBJECT:
        pairs = [(k, norm(x, prefix, sort_keys)) for k, x in v[1]]
        if sort_keys:
            pairs.sort(key=lambda kv: kv[0])
        return (OBJECT, pairs)
    return v


def render(v):
    if isinstance(v, tuple) and v[0] == NUMBER:
        return v[1]
    if isinstance(v, tuple):
        return "{" + ",".join(json.dumps(k, ensure_ascii=False) + ":" + render(x) for k, x in v[1]) + "}"
    if isinstance(v, list):
        return "[" + ",".join(render(x) for x in v) + "]"
    return json.dumps(v, ensure_ascii=False)


def main():
    mrp_json, mrp_dir, nf_json, name = sys.argv[1:5]
    allowlist = sys.argv[5] if len(sys.argv) > 5 else os.path.join(
        os.path.dirname(os.path.abspath(__file__)), "divergences.json")
    rules = excused_rules(allowlist)

    prefix = mrp_dir.rstrip("/") + "/" if STRIP_OUTS_PREFIX in rules else ""
    sort_keys = SORT_OBJECT_KEYS in rules

    mrp = render(norm(load(mrp_json), prefix, sort_keys))
    nf = render(norm(load(nf_json), "", sort_keys))

    if mrp == nf:
        print("  json: match")
        return 0

    print(f"FAIL[{name}]: _outs json mismatch")
    print(f"  mrp(normalized) = {mrp}")
    print(f"  nextflow        = {nf}")
    return 1


if __name__ == "__main__":
    sys.exit(main())
