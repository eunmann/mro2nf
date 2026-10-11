import martian


def split(args):
    # pylint: disable=unused-argument
    return {
        "chunks": [{"i": 0, "__mem_gb": 0.5}, {"i": 1}],
        "join": {"__mem_gb": 0.5},
    }


def main(args, outs):
    outs.m = martian.get_memory_allocation()
    outs.am = args.__mem_gb


def join(args, outs, chunk_defs, chunk_outs):
    # pylint: disable=unused-argument
    outs.cm = [c.m for c in chunk_outs]
    outs.cam = [c.am for c in chunk_outs]
    outs.jm = martian.get_memory_allocation()
    outs.jam = args.__mem_gb
