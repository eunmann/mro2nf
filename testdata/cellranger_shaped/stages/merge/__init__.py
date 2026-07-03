def main(args, outs):
    probe = args.probe_dims if args.probe_dims is not None else 0
    per = sum(args.per_sample) if args.per_sample else 0
    outs.total = args.dims + probe + args.filtered + per
