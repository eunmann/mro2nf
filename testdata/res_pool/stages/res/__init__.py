import os

import martian


def main(args, outs):
    outs.t = martian.get_threads_allocation()
    outs.m = martian.get_memory_allocation()
    outs.at = args.__threads
    outs.am = args.__mem_gb
    outs.avm = args.__vmem_gb
    outs.omp = os.environ.get("OMP_NUM_THREADS")
