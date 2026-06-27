def split(args):
    # Chunk count and per-chunk resources are computed at runtime from the staged
    # input file — not known until the file is read.
    chunks = []
    with open(args.manifest) as handle:
        for line in handle:
            line = line.strip()
            if line:
                n = int(line)
                chunks.append({"n": n, "__threads": n, "__mem_gb": 1})
    return {"chunks": chunks}


def main(args, outs):
    outs.part = args.n * args.n


def join(args, outs, chunk_defs, chunk_outs):
    outs.total = sum(out.part for out in chunk_outs)
    outs.nchunks = len(chunk_outs)
