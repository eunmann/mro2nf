import martian

__MRO__ = """
stage DOUBLE(
    in  int x,
    out int y,
    out txt note,
)
"""


def main(args, outs):
    outs.y = args.x * 2
    path = martian.make_path("note.txt")
    if isinstance(path, bytes):
        path = path.decode("utf-8")
    with open(path, "w") as handle:
        handle.write("x=%d y=%d\n" % (args.x, outs.y))
    outs.note = path
