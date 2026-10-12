import sys

# The adapter serializes the outs recursively, one Python frame per level.
sys.setrecursionlimit(100000)


def main(args, outs):
    nest = []
    for _ in range(args.depth - 1):
        nest = [nest]
    outs.deep = {"nest": nest}
