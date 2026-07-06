import martian

__MRO__ = """
stage MAKEB(
    in  int x,
    out txt report,
    out int n,
)
"""


def main(args, outs):
    path = martian.make_path("report.txt")
    if isinstance(path, bytes):
        path = path.decode("utf-8")
    with open(path, "w") as handle:
        handle.write("n=%d" % args.x)
    outs.report = path
    outs.n = args.x
