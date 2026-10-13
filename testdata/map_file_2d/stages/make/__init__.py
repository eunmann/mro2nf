import martian


def _write(name, text):
    path = martian.make_path(name)
    if isinstance(path, bytes):
        path = path.decode("utf-8")
    with open(path, "w") as handle:
        handle.write(text)
    return path


def main(args, outs):
    outs.rows = [
        [_write("r%d_%d.txt" % (i, j), "row%d:%d" % (i, n)) for j, n in enumerate(row)]
        for i, row in enumerate(args.ns)
    ]
    outs.cols = [[_write("c0_%d.txt" % j, "col0:%d" % n) for j, n in enumerate(args.ns[0])]]
    outs.byk = {
        "k%d" % i: [_write("k%d_%d.txt" % (i, j), "key%d:%d" % (i, n)) for j, n in enumerate(row)]
        for i, row in enumerate(args.ns)
    }
