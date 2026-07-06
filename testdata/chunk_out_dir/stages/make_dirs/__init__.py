__MRO__ = """
stage MAKE_DIRS(
    in  int   n,
    out int   total,
) split using (
    in  int   idx,
    out path  d,
)
"""

import os

import martian


def _path(name):
    p = martian.make_path(name)
    return p.decode("utf-8") if isinstance(p, bytes) else p


def split(args):
    return {"chunks": [{"idx": i, "__threads": 1, "__mem_gb": 1}
                       for i in range(args.n)]}


def main(args, outs):
    # Each chunk emits a DIRECTORY containing one file.
    d = _path("chunkdir%d" % args.idx)
    os.makedirs(d)
    with open(os.path.join(d, "v.txt"), "w") as fh:
        fh.write(str((args.idx + 1) * 10))
    outs.d = d


def join(args, outs, chunk_defs, chunk_outs):
    # pylint: disable=unused-argument
    # Read a file inside each chunk's output DIRECTORY: the directory leaf must
    # have been staged (DirMarker) into the join worker with its contents.
    total = 0
    for co in chunk_outs:
        with open(os.path.join(co.d, "v.txt")) as fh:
            total += int(fh.read().strip())
    outs.total = total
