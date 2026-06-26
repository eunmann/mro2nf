__MRO__ = """
stage INC(
    in  int x,
    out int y,
)
"""


def main(args, outs):
    # If x arrived as the float 5.0 this would be 6.0 (or fail for an int out);
    # the coercion ensures x is the integer 5 so y is 6.
    outs.y = args.x + 1
