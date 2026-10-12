import martian


def main(args, outs):
    outs.line = "total=%d" % args.total
    outs.res = "threads=%s mem=%s args=%s/%s/%s" % (
        martian.get_threads_allocation(),
        martian.get_memory_allocation(),
        args.__threads,
        args.__mem_gb,
        args.__vmem_gb,
    )
