import sys

# The adapter parses the args before main runs, one Python frame per level.
sys.setrecursionlimit(100000)


def main(args, outs):
    depth = 0
    nest = args.deep["nest"]
    while isinstance(nest, list):
        depth += 1
        nest = nest[0] if nest else None
    outs.depth = depth
