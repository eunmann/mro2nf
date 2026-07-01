import martian

__MRO__ = """
stage ASSEMBLE(
    in  txt[]   shards,
    in  txt[]   labels,
    out Report  report,
    out txt[]   gathered,
)
"""


def _path(name):
    p = martian.make_path(name)
    if isinstance(p, bytes):
        p = p.decode("utf-8")
    return p


def _copy(src, dst):
    with open(src) as r, open(dst, "w") as w:
        w.write(r.read())


def main(args, outs):
    # An array of files, re-materialized under this stage.
    shards = []
    for i, src in enumerate(args.shards):
        dst = _path("s%d.txt" % i)
        _copy(src, dst)
        shards.append(dst)

    # A map of files, keyed by name.
    byname = {}
    for i, src in enumerate(args.labels):
        dst = _path("lab_%d.txt" % i)
        _copy(src, dst)
        byname["item%d" % i] = dst

    # A scalar file.
    summary = _path("summary.txt")
    with open(summary, "w") as handle:
        handle.write("shards=%d labels=%d" % (len(args.shards), len(args.labels)))

    # A fresh top-level array (final-only), distinct from the intermediate input.
    gathered = []
    for i, src in enumerate(args.shards):
        dst = _path("g%d.txt" % i)
        _copy(src, dst)
        gathered.append(dst)
    outs.gathered = gathered

    outs.report = {
        "shards": shards,
        "byname": byname,
        "summary": summary,
        "count": len(args.shards) + len(args.labels),
    }
