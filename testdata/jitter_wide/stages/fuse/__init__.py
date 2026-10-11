import random
import time

import martian


def main(args, outs):
    # The random sleep shuffles the order in which forks complete.
    time.sleep(random.random() * 0.4)
    with open(args.payload) as handle:
        head = handle.readline().strip()
    outs.w = args.v * args.v
    path = martian.make_path("f%d.txt" % args.v)
    if isinstance(path, bytes):
        path = path.decode("utf-8")
    with open(path, "w") as handle:
        handle.write("v=%d w=%d %s\n" % (args.v, outs.w, head))
    outs.f = path
