import json


def main(args, outs):
    keys = ["dot", "sub", "up", "f", "t", "arr", "m"]
    outs.seen = json.dumps({k: getattr(args, k) for k in keys}, sort_keys=True)
