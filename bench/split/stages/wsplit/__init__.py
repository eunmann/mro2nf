def split(args):
    # Fan out nchunks chunks. The chunk work (main) does not read `payload`; it is a
    # stage-level arg that the current data plane still broadcasts into every chunk.
    return {"chunks": [{"i": i, "__mem_gb": 1} for i in range(int(args.nchunks))]}


def main(args, outs):
    # Deliberately does NOT read args.payload: the point is that a chunk should not
    # be handed a stage-level file it never consumes.
    outs.part = args.i * args.i


def join(args, outs, chunk_defs, chunk_outs):
    outs.total = sum(out.part for out in chunk_outs)
