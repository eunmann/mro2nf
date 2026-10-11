import os


def main(args, outs):
    outs.name = os.path.basename(args.reads)
