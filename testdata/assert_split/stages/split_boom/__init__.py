import martian


def split(args):
    # pylint: disable=unused-argument
    martian.exit("intentional assertion in split")


def main(args, outs):
    outs.y = args.x


def join(args, outs, chunk_defs, chunk_outs):
    # pylint: disable=unused-argument
    outs.total = sum(out.y for out in chunk_outs)
