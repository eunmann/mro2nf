import martian

__MRO__ = """
stage MAKE_SHARDS(
    in  int    n,
    out txt[]  shards,
) split using (
    in  int    idx,
    out txt    shard,
)
"""


def _path(name):
    p = martian.make_path(name)
    if isinstance(p, bytes):
        p = p.decode("utf-8")
    return p


def split(args):
    return {"chunks": [{"idx": i} for i in range(args.n)]}


def main(args, outs):
    path = _path("shard%d.txt" % args.idx)
    with open(path, "w") as handle:
        handle.write("shard-%d" % args.idx)
    outs.shard = path


def join(args, outs, chunk_defs, chunk_outs):
    outs.shards = [out.shard for out in chunk_outs]
