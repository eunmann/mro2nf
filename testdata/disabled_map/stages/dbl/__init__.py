__MRO__ = """
stage DBL(
    in  int v,
    out int w,
)
"""


def main(args, outs):
    outs.w = args.v * 2
