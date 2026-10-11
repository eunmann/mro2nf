import martian


# BENCH_PROBE is a recognizable marker the benchmark harness greps for to locate
# every on-disk materialization / staging of the per-fork file across the
# run's work dir.
BENCH_PROBE = "MRE_BENCH_MAPDISABLED_PROBE"


def main(args, outs):
    path = martian.make_path("f%d.txt" % args.x)
    if isinstance(path, bytes):
        path = path.decode("utf-8")
    with open(path, "w") as handle:
        handle.write("%s x=%d\n" % (BENCH_PROBE, args.x))
    outs.f = path
