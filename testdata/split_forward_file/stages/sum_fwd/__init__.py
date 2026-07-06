__MRO__ = """
stage SUM_FWD(
    in  txt   seed,
    in  int   n,
    out int   total,
) split using (
    in  int   chunk_id,
    in  txt   ref,
    out int   partial,
)
"""


def split(args):
    # Forward the STAGE-INPUT file into every chunk's args (no new file created).
    return {"chunks": [{"chunk_id": i, "ref": args.seed, "__threads": 1, "__mem_gb": 1}
                       for i in range(args.n)]}


def main(args, outs):
    with open(args.ref) as fh:
        outs.partial = int(fh.read().strip())


def join(args, outs, chunk_defs, chunk_outs):
    # pylint: disable=unused-argument
    # Read the forwarded file off each chunk DEF: the split forwarded an
    # already-staged input path, which the chunk-def bundle must carry to join.
    total = 0
    for cd in chunk_defs:
        with open(cd.ref) as fh:
            total += int(fh.read().strip())
    outs.total = total
