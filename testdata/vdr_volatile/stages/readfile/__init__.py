def main(args, outs):
    with open(args.f) as handle:
        outs.y = float(handle.read().strip()) * 2
