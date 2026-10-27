import os


def main(args, outs):
    outs.names = ",".join(sorted(os.listdir(args.d)))
    with open(os.path.join(args.d, "sub_link", "inner.txt")) as handle:
        outs.inner = handle.read()
