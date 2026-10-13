def _read(paths):
    if paths is None:
        return "null"
    texts = []
    for path in paths:
        with open(path) as handle:
            texts.append(handle.read())
    return ",".join(texts)


def main(args, outs):
    outs.s = _read(args.a) + "|" + _read(args.b)
