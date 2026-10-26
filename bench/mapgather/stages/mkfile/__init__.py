import martian


# BENCH_PROBE is a recognizable marker the benchmark harness greps for to locate
# every on-disk materialization / staging of these files across the run's work dir.
BENCH_PROBE = "MRE_BENCH_MAPGATHER_PROBE"


def main(args, outs):
    path = martian.make_path("part.txt")
    if isinstance(path, bytes):
        path = path.decode("utf-8")
    with open(path, "w") as handle:
        handle.write(BENCH_PROBE + "\n")
        handle.write("z" * int(args.x))
    outs.f = path
