import os

import martian

__MRO__ = """
stage MAKE(
    out json[]    js,
    out Pair      pair,
    out map<txt>  m,
    out path      d,
)
"""


def _path(name):
    path = martian.make_path(name)
    if isinstance(path, bytes):
        path = path.decode("utf-8")
    return path


def _write(name, text):
    path = _path(name)
    with open(path, "w") as handle:
        handle.write(text)
    return path


def main(args, outs):
    first = _write("first.json", '{"n": 1}')
    second = _write("second.json", '{"n": 2}')
    outs.js = [first, None, second, first]
    shared = _write("shared.txt", "shared")
    outs.pair = {"zeta": shared, "alpha": shared}
    outs.m = {"z": shared, "b": _write("b.txt", "b"), "a": shared}
    d = _path("d")
    os.makedirs(d, exist_ok=True)
    with open(os.path.join(d, "inner.txt"), "w") as handle:
        handle.write("inner")
    outs.d = d
