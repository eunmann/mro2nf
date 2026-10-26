import os


def main(args, outs):
    outs.size = os.path.getsize(args.f)
    outs.total = sum(os.path.getsize(f) for f in args.all)
