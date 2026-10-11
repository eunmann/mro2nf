def describe(v):
    return "n=%r(%s)" % (v, type(v).__name__)


def split(args):
    return {
        "chunks": [
            {"n": args.total / 2, "__mem_gb": 1},
            {"n": args.total // 2, "__mem_gb": 1},
        ]
    }


def main(args, outs):
    outs.seen = describe(args.n)


def join(args, outs, chunk_defs, chunk_outs):
    # pylint: disable=unused-argument
    outs.mains = [c.seen for c in chunk_outs]
    outs.defs = [describe(d.n) for d in chunk_defs]
