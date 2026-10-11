import os

import martian

__MRO__ = """
stage MAKES2(
    out S2 o4,
)
"""


def _dir(name, files):
    path = martian.make_path(name)
    if isinstance(path, bytes):
        path = path.decode("utf-8")
    os.makedirs(path, exist_ok=True)
    for f in files:
        with open(os.path.join(path, f), "w") as handle:
            handle.write(name + "/" + f)
    return path


def main(args, outs):
    outs.o4 = {"f4": [{"f0": _dir("a", ["x.txt"])}, {"f0": _dir("b", ["y.txt", "z.txt"])}]}
