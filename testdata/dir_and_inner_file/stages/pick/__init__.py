import os

import martian

__MRO__ = """
stage PICK(
    in  path d,
    out txt  x,
)
"""


def main(args, outs):
    outs.x = os.path.join(args.d, "sub", "x.txt")
