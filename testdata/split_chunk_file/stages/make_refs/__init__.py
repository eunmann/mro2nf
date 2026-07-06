__MRO__ = """
stage MAKE_REFS(
    in  int   n,
    out int   total,
) split using (
    in  int   chunk_id,
    in  bin   ref,
    out int   partial,
)
"""

import martian


def split(args):
    """Create one custom-filetype file per chunk in the split worker's scratch
    and hand its path back as a per-chunk arg (the #217 shape)."""
    chunks = []
    for i in range(args.n):
        ref = martian.make_path("ref_%d.bin" % i).decode()
        with open(ref, "w") as fh:
            fh.write(str((i + 1) * 10))
        chunks.append({"chunk_id": i, "ref": ref, "__threads": 1, "__mem_gb": 1})
    return {"chunks": chunks}


def main(args, outs):
    # main reads the file-typed chunk arg too (this path already worked).
    with open(args.ref) as fh:
        outs.partial = int(fh.read().strip())


def join(args, outs, chunk_defs, chunk_outs):
    # pylint: disable=unused-argument
    # The regression: read the file-typed leaf from each CHUNK DEF (not the
    # chunk outs). On an object store the join worker only has this file if the
    # chunk def was staged; before the #217 fix it held the split's absent
    # scratch path and the open() failed.
    total = 0
    for cd in chunk_defs:
        with open(cd.ref) as fh:
            total += int(fh.read().strip())
    outs.total = total
