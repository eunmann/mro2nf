import martian

__MRO__ = """
stage LABEL(
    in  int  v,
    out txt  labeled,
)
"""


def main(args, outs):
    path = martian.make_path("label.txt")
    if isinstance(path, bytes):
        path = path.decode("utf-8")
    with open(path, "w") as handle:
        handle.write("label-%d" % args.v)
    outs.labeled = path
