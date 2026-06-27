import martian

__MRO__ = """
stage MAKER(
    in  int    k,
    out Report r,
)
"""


def main(args, outs):
    paths = []
    for i in range(args.k):
        p = martian.make_path("r%d.txt" % i)
        if isinstance(p, bytes):
            p = p.decode("utf-8")
        with open(p, "w") as h:
            h.write("report %d" % i)
        paths.append(p)
    outs.r = {"files": paths, "n": args.k}
