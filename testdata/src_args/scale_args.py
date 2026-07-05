#!/usr/bin/env python3
"""An `exec` stage whose src line carries arguments.

argv is <src args...> <phase> <metadata dir> <files dir> <journal file>:
Martian prepends the src args to the metadata protocol args
(martian/core/node.go runChunk, ExecStage arm), and mre's exec path mirrors
that. Unlike exec_min's stub this one also writes journal entries (the
martian_shell.py update_journal protocol), so REAL mrp notices completion
and the golden can be generated from an actual mrp run.
"""
import json
import os
import sys

factor, tag, phase, meta = float(sys.argv[1]), sys.argv[2], sys.argv[3], sys.argv[4]
journal = sys.argv[6]


def write(name, text):
    with open(os.path.join(meta, "_" + name), "w") as f:
        f.write(text)
    # Journal entry: tells the parent mrp its metadata changed
    # (adapters/python/martian_shell.py update_journal). Harmless under mre.
    with open(journal + "." + name, "w") as f:
        f.write("")


with open(os.path.join(meta, "_args")) as f:
    args = json.load(f)

if phase == "main":
    write("outs", json.dumps({"y": args["x"] * factor, "tag": tag}))
    write("complete", "complete")
