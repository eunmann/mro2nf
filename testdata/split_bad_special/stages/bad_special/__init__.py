def split(args):
    return {"chunks": [{"x": x, "__special": 1} for x in args.xs]}


def main(args, outs):
    outs.y = args.x


def join(args, outs, chunk_defs, chunk_outs):
    # pylint: disable=unused-argument
    outs.total = sum(out.y for out in chunk_outs)
