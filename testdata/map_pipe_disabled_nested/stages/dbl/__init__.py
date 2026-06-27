__MRO__ = """
stage DBL(
    in  int x,
    out int y,
)
"""


def main(args, outs):
    outs.y = args.x * 2
