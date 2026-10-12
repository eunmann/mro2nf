import martian

__MRO__ = """
stage WRITEF(
    in  int i,
    out txt f,
)
"""


def main(args, outs):
    path = martian.make_path("v%d.txt" % args.i)
    if isinstance(path, bytes):
        path = path.decode("utf-8")
    with open(path, "w") as handle:
        handle.write("val=%d" % args.i)
    outs.f = path
