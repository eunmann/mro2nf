def main(args, outs):
    outs.a = 1
    outs.b = 2
    outs.c = 3
    with open(outs.extra, "w") as f:
        f.write("extra\n")
