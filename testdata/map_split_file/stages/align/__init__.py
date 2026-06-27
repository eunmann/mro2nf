import martian

__MRO__ = """
stage ALIGN(
    in  int sample,
    out txt aligned,
) split (
    in  int lane,
    out int laneval,
)
"""


def split(args):
    return {"chunks": [{"lane": i, "__mem_gb": 1, "__threads": 1} for i in range(args.sample)]}


def main(args, outs):
    outs.laneval = args.lane * 10


def join(args, outs, chunk_defs, chunk_outs):
    # pylint: disable=unused-argument
    total = sum(o.laneval for o in chunk_outs)
    path = martian.make_path("s%d.txt" % args.sample)
    if isinstance(path, bytes):
        path = path.decode("utf-8")
    with open(path, "w") as handle:
        handle.write("sample=%d total=%d" % (args.sample, total))
    outs.aligned = path
