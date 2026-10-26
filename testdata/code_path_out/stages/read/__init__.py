def main(args, outs):
    with open(args.shipped) as f:
        outs.content = f.read().strip()
