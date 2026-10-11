import martian


def main(args, outs):
    outs.s = args.s
    outs.arr = args.arr
    outs.m = args.m
    outs.lit = args.lit
    path = martian.make_path("made.txt")
    if isinstance(path, bytes):
        path = path.decode("utf-8")
    with open(path, "w") as handle:
        handle.write("made\n")
    outs.made = path
    # A file-typed output set to a marker-shaped string the stage never wrote.
    outs.fake = "@mre:file:f/L0000.txt"
