def main(args, outs):
    with open(outs.reads, "w") as handle:
        handle.write("reads\n")
    with open(outs.reads + ".idx", "w") as handle:
        handle.write("index\n")
