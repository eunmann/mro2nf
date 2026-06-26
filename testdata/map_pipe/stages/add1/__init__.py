__MRO__ = """
stage ADD1(
    in  float x,
    out float y,
)
"""


def main(args, outs):
    outs.y = args.x + 1
