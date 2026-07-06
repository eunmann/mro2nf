__MRO__ = """
stage GEN(
    out txt data,
)
"""

import martian


def main(args, outs):
    p = martian.make_path("data.txt")
    p = p.decode("utf-8") if isinstance(p, bytes) else p
    with open(p, "w") as fh:
        fh.write("10")
    outs.data = p
