#!/usr/bin/env python3
"""An exec stage: raw Martian metadata protocol, y = x * 2."""
import json
import os
import sys

phase, meta = sys.argv[1], sys.argv[2]
with open(os.path.join(meta, "_args")) as f:
    args = json.load(f)
if phase == "main":
    with open(os.path.join(meta, "_outs"), "w") as f:
        json.dump({"y": args["x"] * 2}, f)
    with open(os.path.join(meta, "_complete"), "w") as f:
        f.write("complete")
