import martian


def main(args, outs):
    lanes = {}
    for k in ["laneA", "laneB"]:
        paths = []
        for i in range(2):
            p = martian.make_path(k + str(i) + ".txt")
            if isinstance(p, bytes):
                p = p.decode("utf-8")
            with open(p, "w") as h:
                h.write(k + "-" + str(i))
            paths.append(p)
        lanes[k] = paths
    outs.lanes = lanes
