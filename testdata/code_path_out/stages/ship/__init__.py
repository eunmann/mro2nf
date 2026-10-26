import os


def main(args, outs):
    outs.shipped = os.path.join(os.path.dirname(os.path.abspath(__file__)), "shipped.txt")
