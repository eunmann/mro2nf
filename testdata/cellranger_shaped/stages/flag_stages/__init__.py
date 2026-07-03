def main(args, outs):
    outs.no_probe = args.mode != 1
    outs.disable_legacy = args.mode == 0
    outs.disable_multi = args.mode != 2
