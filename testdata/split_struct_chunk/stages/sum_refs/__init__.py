__MRO__ = """
stage SUM_REFS(
    in  int   n,
    out int   total,
) split using (
    in  int   chunk_id,
    in  Ref   cfg,
    out int   partial,
)
"""

import martian


def split(args):
    """Create a file per chunk and nest its path inside a struct chunk-arg."""
    chunks = []
    for i in range(args.n):
        ref = martian.make_path("ref_%d.bin" % i).decode()
        with open(ref, "w") as fh:
            fh.write(str((i + 1) * 10))
        chunks.append({"chunk_id": i, "cfg": {"ref": ref, "k": i},
                       "__threads": 1, "__mem_gb": 1})
    return {"chunks": chunks}


def main(args, outs):
    # A struct arg is delivered as a dict (subscript access, not attribute).
    with open(args.cfg["ref"]) as fh:
        outs.partial = int(fh.read().strip())


def join(args, outs, chunk_defs, chunk_outs):
    # pylint: disable=unused-argument
    # Read the file leaf nested in the struct chunk-DEF (not the chunk outs):
    # the join worker only has this file if the chunk def's struct was staged
    # and its `ref` leaf descended (#217, nested).
    total = 0
    for cd in chunk_defs:
        with open(cd.cfg["ref"]) as fh:
            total += int(fh.read().strip())
    outs.total = total
