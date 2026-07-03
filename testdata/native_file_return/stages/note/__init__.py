import martian


def main(args, outs):
    with open(outs.note, "w") as f:
        f.write("x=%d\n" % args.x)
