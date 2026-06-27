def main(args, outs):
    # Sum the numbers in the staged input file, then scale. The result depends on
    # the file's content, so it only matches the golden if the file was actually
    # staged into and read by this task.
    total = 0.0
    with open(args.reads) as handle:
        for line in handle:
            line = line.strip()
            if line:
                total += float(line)
    outs.total = total * args.scale
