import json
import os


def split(args):
    with open(args.sample_info) as handle:
        read_path = json.load(handle)["read_path"]
    names = sorted(os.listdir(read_path))
    return {"chunks": [{"read_file": os.path.join(read_path, name)} for name in names]}


def main(args, outs):
    with open(args.read_file) as handle:
        outs.n = sum(int(line) for line in handle if line.strip())


def join(args, outs, chunk_defs, chunk_outs):
    reference = os.path.join(args.reference_info["reference_path"], "genome.fa")
    with open(reference) as handle:
        lines = sum(1 for _ in handle)
    outs.total = sum(out.n for out in chunk_outs) + lines
