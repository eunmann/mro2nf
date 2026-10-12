import martian


def allocation(args):
    return "threads=%s mem=%s args=%s/%s/%s" % (
        martian.get_threads_allocation(),
        martian.get_memory_allocation(),
        args.__threads,
        args.__mem_gb,
        args.__vmem_gb,
    )


def split(args):
    res = "threads=%s mem=%s" % (
        martian.get_threads_allocation(),
        martian.get_memory_allocation(),
    )
    chunks = [{"x": x, "split_res": res} for x in args.xs]
    chunks[0].update({"__threads": 2, "__mem_gb": 2})
    return {"chunks": chunks, "join": {"__mem_gb": 2}}


def main(args, outs):
    outs.sq = args.x * args.x
    outs.res = allocation(args)


def join(args, outs, chunk_defs, chunk_outs):
    outs.total = sum(c.sq for c in chunk_outs)
    outs.split_res = chunk_defs[0].split_res
    outs.chunk_res = [c.res for c in chunk_outs]
    outs.join_res = allocation(args)
