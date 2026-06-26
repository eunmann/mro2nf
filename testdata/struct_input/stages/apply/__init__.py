def main(args, outs):
    outs.r = "%s=%d" % (args.cfg["label"], args.cfg["scale"] * args.x)
