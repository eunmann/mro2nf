import os

import martian

__MRO__ = """
stage MKLINKS(
    out path d,
)
"""


def main(args, outs):
    path = martian.make_path("d")
    if isinstance(path, bytes):
        path = path.decode("utf-8")
    os.makedirs(path)
    with open(os.path.join(path, "real.txt"), "w") as handle:
        handle.write("real")
    os.symlink("real.txt", os.path.join(path, "rel_link.txt"))
    os.symlink("/nonexistent", os.path.join(path, "dangling"))
    outs.d = path
