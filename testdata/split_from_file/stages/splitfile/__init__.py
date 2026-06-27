def split(args):
    # Chunk count and per-chunk resources are computed at runtime from the staged
    # input file — not known until the file is read.
    chunks = []
    with open(args.manifest) as handle:
        for line in handle:
            line = line.strip()
            if line:
                n = int(line)
                # __threads is derived from the data but capped at 2 so a chunk
                # never requests more CPUs than a small CI runner has (the local
                # executor rejects a task whose cpus exceed the machine). The
                # value still varies per chunk; total/nchunks depend on n, not n's
                # thread count, so the goldens are unaffected.
                chunks.append({"n": n, "__threads": min(n, 2), "__mem_gb": 1})
    return {"chunks": chunks}


def main(args, outs):
    outs.part = args.n * args.n


def join(args, outs, chunk_defs, chunk_outs):
    outs.total = sum(out.part for out in chunk_outs)
    outs.nchunks = len(chunk_outs)
