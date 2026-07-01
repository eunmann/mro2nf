#!/usr/bin/env python3
"""Emit one output param as the de-bundle model: a typed sidecar + flat leaves.

This is the producer half of the #13 spike. It proves `mre` can represent a
stage output as a small typed `data.json` that references its file leaves by
ordinal name, writing each real file exactly ONCE (no `f/` byte-copy for
transport) and letting Nextflow stage the leaves as first-class `path` items.

Usage: emit.py <param> <spec.json>

<spec.json> is the typed value tree, with each file leaf written as
{"__leaf__": "<content>"}. emit walks it in canonical order (arrays by index,
maps by sorted key, structs in field order), writes each leaf to a flat file
named "<param>.L<NNNN>", and writes "<param>.sidecar.json" with every leaf
replaced by a marker "@spike:file:<param>.L<NNNN>". The nested shape lives only
in the sidecar; the leaf set is flat, so map<file[]>, struct-of-file-array, and
any deep combination all collapse to "typed tree + flat ordinal file set".
"""
import json
import sys

MARKER = "@spike:file:"


def main():
    param, spec_path = sys.argv[1], sys.argv[2]
    with open(spec_path) as h:
        spec = json.load(h)

    counter = [0]

    def walk(v):
        # A file leaf is the sentinel object; everything else is structure walked
        # deterministically so the flat leaf order is stable across runs (the
        # -resume cache depends on it).
        if isinstance(v, dict) and "__leaf__" in v:
            name = "%s.L%04d" % (param, counter[0])
            counter[0] += 1
            with open(name, "w") as fh:
                fh.write(v["__leaf__"])
            return MARKER + name
        if isinstance(v, list):
            return [walk(e) for e in v]
        if isinstance(v, dict):
            return {k: walk(v[k]) for k in sorted(v)}
        return v

    sidecar = walk(spec)
    with open("%s.sidecar.json" % param, "w") as h:
        json.dump(sidecar, h, indent=2)


if __name__ == "__main__":
    main()
