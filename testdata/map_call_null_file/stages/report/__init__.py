import martian


def main(args, outs):
    if args.v == "none":
        outs.report = {"result": None, "n": 0}
        return
    path = martian.make_path("result.json")
    if isinstance(path, bytes):
        path = path.decode("utf-8")
    with open(path, "w") as f:
        f.write("{}\n")
    outs.report = {"result": path, "n": 1}
