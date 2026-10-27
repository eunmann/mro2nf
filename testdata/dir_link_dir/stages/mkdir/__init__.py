import os

import martian


def main(args, outs):
    path = martian.make_path("d")
    if isinstance(path, bytes):
        path = path.decode("utf-8")
    os.makedirs(os.path.join(path, "sub"))
    with open(os.path.join(path, "sub", "inner.txt"), "w") as handle:
        handle.write("inner")
    os.symlink("sub", os.path.join(path, "sub_link"))
    outs.d = path
