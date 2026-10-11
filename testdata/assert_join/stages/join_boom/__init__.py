import martian


def split(args):
    return {"chunks": [{"x": x} for x in args.xs]}


def main(args, outs):
    outs.y = args.x


def join(args, outs, chunk_defs, chunk_outs):
    # pylint: disable=unused-argument
    martian.exit("intentional assertion in join")
