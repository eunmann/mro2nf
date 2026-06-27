def main(args, outs):
    total = 0
    for path in args.fs:
        with open(path) as h: total += int(h.read().strip())
    outs.total = total
