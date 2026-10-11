def split(args):
    # pylint: disable=unused-argument
    return {"chunks": []}


def main(args, outs):
    outs.y = args.x


def join(args, outs, chunk_defs, chunk_outs):
    # pylint: disable=unused-argument
    outs.total = len(chunk_defs) + len(chunk_outs)
