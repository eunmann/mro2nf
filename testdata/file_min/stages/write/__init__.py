import martian


def main(args, outs):
    path = martian.make_path("note.txt")
    if isinstance(path, bytes):
        path = path.decode("utf-8")
    with open(path, "w") as handle:
        handle.write("x=%s\n" % args.x)
    outs.note = path
