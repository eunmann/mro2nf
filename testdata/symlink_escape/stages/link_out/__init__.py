import os

import martian


def main(args, outs):
    path = martian.make_path("linked.txt")
    if isinstance(path, bytes):
        path = path.decode("utf-8")
    os.symlink(os.path.abspath(__file__), path)
    outs.linked = path
