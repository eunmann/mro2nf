__MRO__ = """
stage SHARD(
    in  int    n,
    out int    total,
) split using (
    in  int    idx,
    out txt[]  parts,
)
"""

import martian


def _path(name):
    p = martian.make_path(name)
    return p.decode("utf-8") if isinstance(p, bytes) else p


def split(args):
    return {"chunks": [{"idx": i, "__threads": 1, "__mem_gb": 1}
                       for i in range(args.n)]}


def main(args, outs):
    # Each chunk writes TWO files (values idx*2+j+1) and returns them as an
    # array chunk-out.
    parts = []
    for j in range(2):
        p = _path("s%d_%d.txt" % (args.idx, j))
        with open(p, "w") as fh:
            fh.write(str(args.idx * 2 + j + 1))
        parts.append(p)
    outs.parts = parts


def join(args, outs, chunk_defs, chunk_outs):
    # pylint: disable=unused-argument
    # Read every file in every chunk's array chunk-out: each element must have
    # been staged into the join worker (array-of-files at the chunk boundary).
    total = 0
    for co in chunk_outs:
        for part in co.parts:
            with open(part) as fh:
                total += int(fh.read().strip())
    outs.total = total
