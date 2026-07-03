import os

__MRO__ = """
stage SPLIT_DEF_FILE(
    in  int   n,
    out int   total,
    src py    "stages/splitdef",
) split using (
    in  file  shared,
    in  int   v,
    out int   part,
)
"""


def split(args):
    # The split builds a file that every chunk (and the join) must read.
    with open("shared.txt", "w") as handle:
        handle.write("100")
    shared = os.path.abspath("shared.txt")
    return {"chunks": [{"shared": shared, "v": i} for i in range(args.n)]}


def main(args, outs):
    with open(args.shared) as handle:
        base = int(handle.read())
    outs.part = base + args.v


def join(args, outs, chunk_defs, chunk_outs):
    # The join reads the split-produced file via the chunk def — the pattern that
    # breaks on a no-shared-filesystem backend unless the def file is staged.
    with open(chunk_defs[0].shared) as handle:
        base = int(handle.read())
    outs.total = base + sum(out.part for out in chunk_outs)
