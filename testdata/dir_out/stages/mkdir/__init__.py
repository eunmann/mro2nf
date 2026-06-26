import os

import martian

__MRO__ = """
stage MKDIR(
    in  int  n,
    out path d,
)
"""


def main(args, outs):
    path = martian.make_path("d")
    if isinstance(path, bytes):
        path = path.decode("utf-8")
    os.makedirs(path, exist_ok=True)
    for i in range(args.n):
        with open(os.path.join(path, "f%d.txt" % i), "w") as handle:
            handle.write("item %d" % i)
    outs.d = path
