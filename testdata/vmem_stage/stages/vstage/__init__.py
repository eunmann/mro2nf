def split(args):
    return {"chunks": [{"value": v, "__mem_gb": 1, "__threads": 1} for v in args.values]}


def main(args, outs):
    outs.square = args.value * args.value


def join(args, outs, chunk_defs, chunk_outs):
    outs.sum = sum(o.square for o in chunk_outs)
