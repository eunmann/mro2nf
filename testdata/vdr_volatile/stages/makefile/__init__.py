import martian


def main(args, outs):
    path = martian.make_path("f.txt")
    if isinstance(path, bytes):
        path = path.decode("utf-8")
    with open(path, "w") as handle:
        handle.write(str(args.x))
    outs.f = path
