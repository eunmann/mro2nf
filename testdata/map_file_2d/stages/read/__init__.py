def main(args, outs):
    texts = []
    for path in args.row:
        with open(path) as handle:
            texts.append(handle.read())
    outs.s = ",".join(texts)
