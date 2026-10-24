import os


def main(args, outs):
    outs.size = os.path.getsize(args.f)
