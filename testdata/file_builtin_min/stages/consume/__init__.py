__MRO__ = """
stage CONSUME(
    in  file blob,
    out int  n,
)
"""


def main(args, outs):
    with open(args.blob) as fh:
        outs.n = int(fh.read().strip())
