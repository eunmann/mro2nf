def main(args, outs):
    with open(args.f) as handle:
        handle.readline()
    outs.w = args.v * args.v
