def main(args, outs):
    total = 0.0
    for path in args.reads.values():
        with open(path) as handle:
            for line in handle:
                line = line.strip()
                if line:
                    total += float(line)
    outs.total = total * args.scale
