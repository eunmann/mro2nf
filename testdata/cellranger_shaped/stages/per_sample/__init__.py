def split(args):
    # Per-sample CHUNKED work (the real CellRanger per-sample shape): one chunk
    # per unit of the sample, so the map call over PER_SAMPLE composes forks
    # (samples) with chunks (units) — a split stage under a map call. The metric
    # is preserved (sample * 10 = sample chunks each contributing 10), so the
    # pipeline output is unchanged.
    return {"chunks": [{"unit": i, "__threads": 1, "__mem_gb": 1} for i in range(args.sample)]}


def main(args, outs):
    outs.partial = 10


def join(args, outs, chunk_defs, chunk_outs):
    outs.metric = sum(c.partial for c in chunk_outs)
