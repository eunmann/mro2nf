import json


def split(args):
    return {"chunks": [{"i": 0}, {"i": 1}]}


def main(args, outs):
    outs.seen = json.dumps(args.subs)


def join(args, outs, chunk_defs, chunk_outs):
    outs.seen = json.dumps([args.subs, [c.seen for c in chunk_outs]])
