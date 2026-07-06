__MRO__ = """
stage MK(
    out file blob,
)
"""

import martian


def main(args, outs):
    p = martian.make_path("blob")
    p = p.decode("utf-8") if isinstance(p, bytes) else p
    with open(p, "w") as fh:
        fh.write("42")
    outs.blob = p
