def main(args, outs):
    with open(args.data) as handle:
        total = int(handle.read().strip())
    for part in args.parts:
        with open(part) as handle:
            total += int(handle.read().strip())
    outs.total = total
