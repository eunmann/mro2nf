#!/usr/bin/env python3
"""A `comp`-style stage: speaks the raw Martian protocol (run via mrjob)."""
import json
import os
import sys

phase, meta = sys.argv[1], sys.argv[2]


def read(name):
    with open(os.path.join(meta, "_" + name)) as f:
        return json.load(f)


def write(name, obj):
    with open(os.path.join(meta, "_" + name), "w") as f:
        json.dump(obj, f)


args = read("args")
if phase == "split":
    write("stage_defs", {
        "chunks": [{"value": v, "__mem_gb": 1, "__threads": 1} for v in args["values"]]
    })
elif phase == "main":
    write("outs", {"sum": None, "square": args["value"] ** 2})
elif phase == "join":
    write("outs", {"sum": sum(c["square"] for c in read("chunk_outs"))})

with open(os.path.join(meta, "_complete"), "w") as f:
    f.write("ok")
