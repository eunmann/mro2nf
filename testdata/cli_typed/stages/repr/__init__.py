def describe(name, value):
    return "%s=%r (%s)" % (name, value, type(value).__name__)


def main(args, outs):
    outs.desc = " ".join(
        describe(name, getattr(args, name)) for name in ("n", "f", "g", "b")
    )
    outs.gated = "enabled" if args.b else "disabled"
