__MRO__ = """
stage ECHO(
    in  int    neg,
    in  int    big,
    in  string esc,
    out int    neg2,
    out int    big2,
    out string esc2,
)
"""


def main(args, outs):
    outs.neg2 = args.neg
    outs.big2 = args.big
    outs.esc2 = args.esc
