import martian


# BENCH_PROBE is a recognizable marker the benchmark harness greps for to locate
# every on-disk materialization / staging of these files across the run's work dir.
BENCH_PROBE = "MRE_BENCH_ZIPGATED_PROBE"


def main(args, outs):
    chunk = ("%s " % BENCH_PROBE) + ("z" * 1024)
    paths = []
    for i in range(int(args.n)):
        path = martian.make_path("part%d.txt" % i)
        if isinstance(path, bytes):
            path = path.decode("utf-8")
        with open(path, "w") as handle:
            handle.write(BENCH_PROBE + "\n")
            for _ in range(int(args.kb)):
                handle.write(chunk)
        paths.append(path)

    outs.fs = paths
