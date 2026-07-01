import martian


# BENCH_PROBE is a recognizable marker the benchmark harness greps for to locate
# every on-disk materialization / staging of this file across the run's work dir.
BENCH_PROBE = "MRE_BENCH_CHAIN_PROBE"


def main(args, outs):
    path = martian.make_path("big.txt")
    if isinstance(path, bytes):
        path = path.decode("utf-8")

    chunk = ("%s " % BENCH_PROBE) + ("x" * 1024)
    with open(path, "w") as handle:
        handle.write(BENCH_PROBE + "\n")
        for _ in range(int(args.kb)):
            handle.write(chunk)

    outs.f = path
