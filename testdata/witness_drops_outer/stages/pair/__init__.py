def main(args, outs):
    outs.z = args.y * 10 + (0 if args.extra is None else args.extra)
