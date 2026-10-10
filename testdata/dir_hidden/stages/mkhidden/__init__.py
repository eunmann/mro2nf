import os

import martian

__MRO__ = """
stage MKHIDDEN(
    in  string tag,
    out path   d,
)
"""


def main(args, outs):
    path = martian.make_path("d")
    if isinstance(path, bytes):
        path = path.decode("utf-8")
    os.makedirs(os.path.join(path, "sub"))
    os.makedirs(os.path.join(path, "empty"))
    with open(os.path.join(path, ".hidden"), "w") as handle:
        handle.write("hidden %s" % args.tag)
    with open(os.path.join(path, "sub", ".x"), "w") as handle:
        handle.write("x %s" % args.tag)
    outs.d = path
