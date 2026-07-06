def main(args, outs):
    total = 0
    for m in args.ms:
        for key in m:
            total += m[key]
    outs.total = total
