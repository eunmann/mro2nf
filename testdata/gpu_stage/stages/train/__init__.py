def split(args):
    return {"chunks": [{"chunk": v, "__mem_gb": 1, "__threads": 1} for v in args.data]}


def main(args, outs):
    outs.part = args.chunk


def join(args, outs, chunk_defs, chunk_outs):
    outs.model = sum(o.part for o in chunk_outs)
