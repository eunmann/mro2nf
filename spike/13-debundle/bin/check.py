#!/usr/bin/env python3
"""Reconstruct de-bundle inputs and assert the leaf contents survived staging.

This is the consumer half of the #13 spike. It takes one or more staged inputs,
each a (sidecar, leaf-dir) pair, resolves every "@spike:file:<name>" marker to a
real file inside that input's own directory, and rebuilds the typed tree with
each leaf replaced by its file content. It then asserts the result equals the
expected tree — proving Nextflow staged each leaf as an individual `path` item
and that multiple inputs do not collide because each resolves within its own
staged directory.

Usage: check.py <label> <expected.json> <sidecar1> <leafdir1> [<sidecar2> <leafdir2> ...]

<expected.json> is a list, one expected tree per input (file leaves given as raw
content strings). Prints "OK[<label>]" on match, "FAIL[...]" and exits 1 otherwise.
"""
import json
import os
import sys

MARKER = "@spike:file:"


def resolve(v, leafdir):
    if isinstance(v, str) and v.startswith(MARKER):
        path = os.path.join(leafdir, v[len(MARKER):])
        with open(path) as h:
            return h.read()
    if isinstance(v, list):
        return [resolve(e, leafdir) for e in v]
    if isinstance(v, dict):
        return {k: resolve(e, leafdir) for k, e in v.items()}
    return v


def main():
    label = sys.argv[1]
    with open(sys.argv[2]) as h:
        expected = json.load(h)

    pairs = sys.argv[3:]
    got = []
    for i in range(0, len(pairs), 2):
        with open(pairs[i]) as h:
            sidecar = json.load(h)
        got.append(resolve(sidecar, pairs[i + 1]))

    if got == expected:
        print("OK[%s]" % label)
        return 0
    print("FAIL[%s]" % label)
    print("  expected:", json.dumps(expected))
    print("  got:     ", json.dumps(got))
    return 1


if __name__ == "__main__":
    sys.exit(main())
