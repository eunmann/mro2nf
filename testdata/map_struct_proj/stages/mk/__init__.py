__MRO__ = """
stage MK(
    in  int    n,
    out map<P> m,
)
"""


def main(args, outs):
    outs.m = {
        "a": {"x": 10 * args.n, "y": args.n},
        "b": {"x": 15 * args.n, "y": args.n},
    }
