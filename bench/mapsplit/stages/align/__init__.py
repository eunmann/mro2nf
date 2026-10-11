import martian


# BENCH_PROBE is a recognizable marker the benchmark harness greps for to locate
# every on-disk materialization / staging of the per-sample file across the
# run's work dir.
BENCH_PROBE = "MRE_BENCH_MAPSPLIT_PROBE"


def split(args):
    return {"chunks": [{"lane": i, "__mem_gb": 1} for i in range(int(args.nlanes))]}


def main(args, outs):
    outs.laneval = args.lane * 10


def join(args, outs, chunk_defs, chunk_outs):
    # pylint: disable=unused-argument
    total = sum(o.laneval for o in chunk_outs)
    path = martian.make_path("s%d.txt" % args.sample)
    if isinstance(path, bytes):
        path = path.decode("utf-8")
    with open(path, "w") as handle:
        handle.write("%s sample=%d total=%d\n" % (BENCH_PROBE, args.sample, total))
    outs.aligned = path
