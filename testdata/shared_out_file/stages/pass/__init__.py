__MRO__ = """
stage PASS(
    in  txt x,
    out txt y,
)
"""


def main(args, outs):
    outs.y = args.x
