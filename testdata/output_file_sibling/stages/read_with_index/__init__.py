def main(args, outs):
    with open(args.reads, "rb") as handle:
        handle.read()
    with open(args.reads + ".idx", "rb") as handle:
        outs.index_bytes = len(handle.read(4))
