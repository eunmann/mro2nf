import json


def main(args, outs):
    with open(outs.bam, "w") as f:
        json.dump({"reads": args.filtered}, f)
