def main(args, outs):
    # Never runs: the bench disables USE in every fork. It exists so the
    # disabled call has real stage code behind it, as in a production pipeline.
    with open(args.f) as handle:
        outs.n = len(handle.read())
