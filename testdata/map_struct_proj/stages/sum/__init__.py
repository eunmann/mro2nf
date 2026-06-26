__MRO__ = """
stage SUM(
    in  map<int> xs,
    out int      total,
)
"""


def main(args, outs):
    outs.total = sum(args.xs.values())
