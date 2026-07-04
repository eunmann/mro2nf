import martian

__MRO__ = """
stage GEN(
    in  int    n,
    out txt[]  files,
)
"""


def main(args, outs):
    outs.files = []
    for i in range(args.n):
        path = martian.make_path("g%d.txt" % i)
        if isinstance(path, bytes):
            path = path.decode("utf-8")
        with open(path, "w") as handle:
            handle.write("x" * (i + 1))
        outs.files.append(path)
