def split(args):
    return {"chunks": [{"chunk": i} for i in range(args.gem_wells)]}


def main(args, outs):
    outs.partial = (args.chunk + 1) * args.reads


def join(args, outs, chunk_defs, chunk_outs):
    outs.counts = sum(c.partial for c in chunk_outs)
