def order(v):
    """The key order of every object in v, depth first."""
    if isinstance(v, dict):
        return "{" + ",".join(k + order(x) for k, x in v.items()) + "}"
    if isinstance(v, list):
        return "[" + ",".join(order(x) for x in v) + "]"
    return ""


def main(args, outs):
    names = ["s1", "s2", "b1", "b2", "s3", "m1", "b3", "s4", "arr", "mp", "b4", "ms", "mk"]
    outs.seen = " ".join(n + "=" + order(getattr(args, n)) for n in names)
    outs.ob = {"q": {"z": 1, "a": 2}, "b": 3}
    outs.om = {"y": 1, "b": {"d": 1, "c": 2}}
