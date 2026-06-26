__MRO__ = """
stage SUMSQ(
    in  float[] values,
    out float   total,
) split (
    in  float   value,
    out float   sq,
)
"""


def split(args):
    return {
        "chunks": [{"value": x, "__threads": 1, "__mem_gb": 1} for x in args.values]
    }


def main(args, outs):
    outs.sq = args.value ** 2


def join(args, outs, chunk_defs, chunk_outs):
    # pylint: disable=unused-argument
    outs.total = sum([out.sq for out in chunk_outs])
