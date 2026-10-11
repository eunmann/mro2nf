import martian


def split(args):
    # pylint: disable=unused-argument
    return {
        "chunks": [{"i": 0, "__threads": 0.25}, {"i": 1}],
        "join": {"__threads": 1.5},
    }


def main(args, outs):
    outs.t = martian.get_threads_allocation()
    outs.at = args.__threads


def join(args, outs, chunk_defs, chunk_outs):
    # pylint: disable=unused-argument
    outs.ct = [c.t for c in chunk_outs]
    outs.cat = [c.at for c in chunk_outs]
    outs.jt = martian.get_threads_allocation()
    outs.jat = args.__threads
