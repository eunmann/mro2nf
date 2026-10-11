import os

import martian


def split(args):
    chunks = []
    for i in range(args.n):
        ref = martian.make_path("ref_%d.txt" % i)
        if isinstance(ref, bytes):
            ref = ref.decode("utf-8")
        with open(ref, "w") as handle:
            handle.write("ref %d\n" % i)
        chunks.append({"ref": ref, "__mem_gb": 1})
    return {"chunks": chunks}


def main(args, outs):
    outs.ref_name = os.path.basename(args.ref)
    with open(outs.part, "w") as handle:
        handle.write(outs.ref_name + "\n")


def join(args, outs, chunk_defs, chunk_outs):
    # pylint: disable=unused-argument
    base = os.path.basename
    outs.def_names = [base(d.ref) for d in chunk_defs]
    outs.main_names = [o.ref_name for o in chunk_outs]
    outs.out_names = [base(o.part) for o in chunk_outs]
    outs.parts = [o.part for o in chunk_outs]
