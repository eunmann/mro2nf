import os


def main(args, outs):
    # Sum the numbers across every file in the staged directory, then scale.
    total = 0.0
    for name in sorted(os.listdir(args.fastqs)):
        with open(os.path.join(args.fastqs, name)) as handle:
            for line in handle:
                line = line.strip()
                if line:
                    total += float(line)
    outs.total = total * args.scale
