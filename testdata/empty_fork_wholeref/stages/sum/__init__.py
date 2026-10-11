import json


def main(args, outs):
    outs.seen = json.dumps(args.pairs)
