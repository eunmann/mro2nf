import martian

__MRO__ = """
stage MAKE2D(
    in  int   n,
    out txt[] row,
)
"""


def _path(name):
    path = martian.make_path(name)
    if isinstance(path, bytes):
        path = path.decode("utf-8")
    return path


def main(args, outs):
    row = []
    for i in range(args.n):
        path = _path("cell%d.txt" % i)
        with open(path, "w") as handle:
            handle.write("n=%d i=%d" % (args.n, i))
        row.append(path)
    outs.row = row + [None, _path("never_written.txt")]
