import os


def main(args, outs):
    base = os.path.basename
    outs.seen = {
        "named": base(args.named),
        "plain": base(args.plain),
        "lanes": [base(p) for p in args.lanes],
        "by_key": {k: base(v) for k, v in args.by_key.items()},
        "sample_reads": base(args.sample["reads"]),
        "entry": base(args.entry),
    }
