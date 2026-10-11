def main(args, outs):
    # exercises a downstream stage consuming the folded null
    outs.w = -1 if args.y is None else args.y * 2
