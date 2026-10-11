import martian

__MRO__ = """
stage MAKE2D(
    in  int   n,
    out txt[] row,
)
"""


def main(args, outs):
    row = []
    for i in range(args.n):
        path = martian.make_path("cell%d.txt" % i)
        if isinstance(path, bytes):
            path = path.decode("utf-8")
        with open(path, "w") as handle:
            handle.write("n=%d i=%d" % (args.n, i))
        row.append(path)
    outs.row = row + [None]
