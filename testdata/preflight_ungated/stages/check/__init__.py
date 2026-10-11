def main(args, outs):
    if args.x <= 0:
        raise ValueError("x must be positive, got %d" % args.x)
