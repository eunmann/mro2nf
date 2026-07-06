def main(args, outs):
    outs.m = {
        "a": {"x": args.n, "y": args.n * 10},
        "b": {"x": args.n * 100, "y": 0},
    }
