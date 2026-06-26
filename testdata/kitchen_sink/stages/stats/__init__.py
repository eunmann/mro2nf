def main(args, outs):
    values = args.values
    count = len(values)
    total = float(sum(values))
    outs.stats = {
        "count": count,
        "total": total,
        "mean": total / count if count else 0.0,
    }
