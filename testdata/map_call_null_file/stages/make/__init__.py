def main(args, outs):
    if args.v == "none":
        outs.result = None
        return
    with open(outs.result, "w") as f:
        f.write("{}\n")
