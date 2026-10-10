def main(args, outs):
    outs.a = args.x
    outs.n = args.x * 10
    with open(outs.report, "w") as f:
        f.write("x=%d\n" % args.x)
