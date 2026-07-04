import martian

__MRO__ = """
stage PROC(
    in  txt f,
    out int size,
)
"""


def main(args, outs):
    with open(args.f) as handle:
        outs.size = len(handle.read())
