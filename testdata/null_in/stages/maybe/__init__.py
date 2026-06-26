def main(args, outs):
    outs.msg = "null" if args.x is None else ("val=%d" % args.x)
