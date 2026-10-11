import martian


# BENCH_PROBE is a recognizable marker the benchmark harness greps for to locate
# every on-disk materialization / staging of the per-leaf file across the
# run's work dir.
BENCH_PROBE = "MRE_BENCH_MAPNESTED_PROBE"


def main(args, outs):
    path = martian.make_path("c%d_%d.txt" % (args.s, args.l))
    if isinstance(path, bytes):
        path = path.decode("utf-8")
    with open(path, "w") as handle:
        handle.write("%s s=%d l=%d\n" % (BENCH_PROBE, args.s, args.l))
    outs.f = path
