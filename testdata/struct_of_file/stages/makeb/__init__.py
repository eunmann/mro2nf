import martian

__MRO__ = """
stage MAKEB(
    in  int    x,
    out Bundle b,
)
"""


def main(args, outs):
    path = martian.make_path("report.txt")
    if isinstance(path, bytes):
        path = path.decode("utf-8")
    with open(path, "w") as handle:
        handle.write("n=%d" % args.x)
    outs.b = {"report": path, "n": args.x}
