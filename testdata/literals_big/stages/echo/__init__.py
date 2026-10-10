def describe(v):
    if isinstance(v, list):
        return "[" + ", ".join(describe(e) for e in v) + "]"
    return "%s %r" % (type(v).__name__, v)


def main(args, outs):
    outs.entry_big2 = describe(args.entry_big)
    outs.entry_huge2 = describe(args.entry_huge)
    outs.entry_arr2 = describe(args.entry_arr)
    outs.call_big2 = describe(args.call_big)
    outs.call_arr2 = describe(args.call_arr)
