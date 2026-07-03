import martian


def _write(name, text):
    path = martian.make_path(name)
    if isinstance(path, bytes):
        path = path.decode("utf-8")
    with open(path, "w") as handle:
        handle.write(text)
    return path


def main(args, outs):
    outs.data = _write("data.txt", "%d" % (args.n * 10))
    outs.parts = [_write("p0.txt", "1"), _write("p1.txt", "2")]
