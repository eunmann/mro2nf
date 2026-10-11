import martian


def main(args, outs):
    path = martian.make_path("payload.txt")
    if isinstance(path, bytes):
        path = path.decode("utf-8")
    with open(path, "w") as handle:
        handle.write("payload\n")
        handle.write("z" * 1024 * int(args.kb))
    outs.f = path
