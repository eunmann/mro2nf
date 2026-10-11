import random
import time

import martian


def split(args):
    return {"chunks": [{"i": i, "w": w, "__mem_gb": 1} for i, w in enumerate(args.ws)]}


def main(args, outs):
    # The random sleep shuffles the order in which chunks complete; the chunk
    # index weights each part, so a reordered fork gather changes the total.
    time.sleep(random.random() * 0.4)
    outs.part = (args.i + 1) * args.w


def join(args, outs, chunk_defs, chunk_outs):
    time.sleep(random.random() * 0.4)
    outs.total = sum(o.part for o in chunk_outs)
    path = martian.make_path("summary.txt")
    if isinstance(path, bytes):
        path = path.decode("utf-8")
    with open(path, "w") as handle:
        for d, o in zip(chunk_defs, chunk_outs):
            handle.write("%d %d %d\n" % (d.i, d.w, o.part))
    outs.summary = path
