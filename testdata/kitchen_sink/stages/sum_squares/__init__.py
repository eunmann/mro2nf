import martian


def split(args):
    return {
        "chunks": [
            {"value": v, "__mem_gb": 1, "__threads": 1} for v in args.values
        ]
    }


def main(args, outs):
    outs.square = args.value ** 2


def join(args, outs, chunk_defs, chunk_outs):
    outs.sum = sum(out.square for out in chunk_outs)
    path = martian.make_path("report.txt")
    if isinstance(path, bytes):
        path = path.decode("utf-8")
    with open(path, "w") as handle:
        handle.write("sum of squares = %s\n" % outs.sum)
    outs.report = path
