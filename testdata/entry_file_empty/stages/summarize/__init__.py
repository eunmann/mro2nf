def _sum(path):
    total = 0.0
    with open(path) as handle:
        for line in handle:
            line = line.strip()
            if line:
                total += float(line)
    return total


def _shape(value):
    if value is None:
        return "None"
    if isinstance(value, (list, dict)):
        return "%s:%d" % (type(value).__name__, len(value))
    return type(value).__name__


def main(args, outs):
    paths = list(args.reads or []) + list((args.named or {}).values())
    ref = args.cfg["ref"]
    if ref is not None:
        paths.append(ref)
    outs.desc = "reads=%s named=%s ref=%s n=%r" % (
        _shape(args.reads), _shape(args.named), _shape(ref), args.cfg["n"])
    outs.total = sum(_sum(p) for p in paths) * args.cfg["n"]
