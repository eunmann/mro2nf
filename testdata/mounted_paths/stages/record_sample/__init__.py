import json


def main(args, outs):
    with open(outs.sample_info, "w") as handle:
        json.dump({"read_path": args.fastqs}, handle)
