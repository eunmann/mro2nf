def main(args, outs):
    outs.total = sum(sum(row) for row in args.grid)
