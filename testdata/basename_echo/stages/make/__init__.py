import os

import martian


def _path(name):
    path = martian.make_path(name)
    if isinstance(path, bytes):
        path = path.decode("utf-8")
    os.makedirs(os.path.dirname(path), exist_ok=True)
    return path


def _write(path, text):
    with open(path, "w") as handle:
        handle.write(text + "\n")
    return path


def main(args, outs):
    # pylint: disable=unused-argument
    _write(outs.named, "named")
    _write(outs.plain, "plain")
    outs.lanes = [_write(_path("lane%d/reads.fastq" % i), "lane%d" % i) for i in (1, 2)]
    outs.by_key = {k: _write(_path(k + "/data.txt"), k) for k in ("k1", "k2")}
    outs.sample = {"name": "s1", "reads": _write(_path("s1_R1.fastq"), "s1")}
