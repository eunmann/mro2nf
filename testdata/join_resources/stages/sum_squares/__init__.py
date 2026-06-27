__MRO__ = """
stage SUM_SQUARES(
    in  float[] values,
    out float   sum,
) split using (
    in  float   value,
    out float   square,
)
"""


def split(args):
    """One chunk per value; the split also requests elevated JOIN resources
    (the gather is memory-hungry), exercising the split-returned join override."""
    return {
        "chunks": [
            {"value": x, "__threads": 1, "__mem_gb": 1} for x in args.values
        ],
        "join": {"__threads": 2, "__mem_gb": 3},
    }


def main(args, outs):
    outs.square = args.value ** 2


def join(args, outs, chunk_defs, chunk_outs):
    # pylint: disable=unused-argument
    outs.sum = sum([out.square for out in chunk_outs])
