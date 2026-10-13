def main(args, outs):
    with open(args.f) as handle:
        outs.n = len(handle.read())
