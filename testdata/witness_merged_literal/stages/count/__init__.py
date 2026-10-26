def main(args, outs):
    outs.n = sum(sum(ys) for ys in args.yss)
