import os

import martian

__MRO__ = """
stage MKDIR(
    out path d,
)
"""


def main(args, outs):
    path = martian.make_path("d")
    if isinstance(path, bytes):
        path = path.decode("utf-8")
    os.makedirs(os.path.join(path, "sub"), exist_ok=True)
    with open(os.path.join(path, "sub", "x.txt"), "w") as handle:
        handle.write("x")
    with open(os.path.join(path, "y.txt"), "w") as handle:
        handle.write("y")
    outs.d = path
