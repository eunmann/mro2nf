import os

import martian


def split(args):
    # pylint: disable=unused-argument
    return {
        "chunks": [
            {"i": 0, "__threads": -3, "__mem_gb": -5},
            {"i": 1, "__threads": 3.5, "__mem_gb": 4.5},
            {"i": 2},
        ],
        "join": {"__threads": -1, "__mem_gb": -8, "__vmem_gb": -2},
    }


def main(args, outs):
    outs.t = martian.get_threads_allocation()
    outs.m = martian.get_memory_allocation()
    outs.avm = args.__vmem_gb
    outs.omp = os.environ.get("OMP_NUM_THREADS")


def join(args, outs, chunk_defs, chunk_outs):
    # pylint: disable=unused-argument
    outs.ct = [c.t for c in chunk_outs]
    outs.cm = [c.m for c in chunk_outs]
    outs.cavm = [c.avm for c in chunk_outs]
    outs.comp = [c.omp for c in chunk_outs]
    outs.jt = martian.get_threads_allocation()
    outs.jm = martian.get_memory_allocation()
    outs.javm = args.__vmem_gb
    outs.jomp = os.environ.get("OMP_NUM_THREADS")
