def main(args, outs):
    counts = {}
    for w in args.words:
        counts[w] = counts.get(w, 0) + 1
    outs.counts = counts
