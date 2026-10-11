import martian


def main(args, outs):
    outs.m = martian.get_memory_allocation()
    outs.am = args.__mem_gb
    outs.avm = args.__vmem_gb
