def split(args):
    return {"chunks": [{"lane": i, "__mem_gb": 1} for i in range(2)]}


def main(args, outs):
    with open(args.ref) as handle:
        handle.readline()
    outs.laneval = args.sample * 10 + args.lane


def join(args, outs, chunk_defs, chunk_outs):
    # pylint: disable=unused-argument
    with open(args.ref) as handle:
        handle.readline()
    outs.total = sum(o.laneval for o in chunk_outs)
