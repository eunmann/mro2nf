def main(args, outs):
    outs.n = -1 if args.ys is None else sum(args.ys)
