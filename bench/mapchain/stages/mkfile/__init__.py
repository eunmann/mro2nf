import martian


# BENCH_PROBE is a recognizable marker the benchmark harness greps for to locate
# every on-disk materialization / staging of these files across the run's work dir.
BENCH_PROBE = "MRE_BENCH_MAPCHAIN_PROBE"


def main(args, outs):
    chunk = ("%s " % BENCH_PROBE) + ("z" * 1024)
    path = martian.make_path("part.txt")
    if isinstance(path, bytes):
        path = path.decode("utf-8")
    with open(path, "w") as handle:
        handle.write(BENCH_PROBE + "\n")
        for _ in range(int(args.kb)):
            handle.write(chunk)
    outs.f = path
