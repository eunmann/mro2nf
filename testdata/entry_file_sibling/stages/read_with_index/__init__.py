import os


def main(args, outs):
    with open(args.reads, "rb") as handle:
        handle.read()
    companion = os.path.join(os.path.dirname(args.reads), args.companion)
    with open(companion, "rb") as handle:
        outs.index_bytes = len(handle.read(4))
