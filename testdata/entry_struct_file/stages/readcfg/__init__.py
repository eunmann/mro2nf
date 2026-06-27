def main(args, outs):
    # Sum the numbers in the struct's file field, scaled by its int field. The
    # Martian adapter passes a struct as a plain dict, so fields are subscripted.
    total = 0.0
    with open(args.cfg["ref"]) as handle:
        for line in handle:
            line = line.strip()
            if line:
                total += float(line)
    outs.total = total * args.cfg["n"]
