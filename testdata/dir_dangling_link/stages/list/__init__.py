import os

__MRO__ = """
stage LIST(
    in  path   d,
    out string names,
    out string target,
)
"""


def main(args, outs):
    outs.names = ",".join(sorted(os.listdir(args.d)))
    link = os.path.join(args.d, "dangling")
    outs.target = os.readlink(link) if os.path.islink(link) else None
