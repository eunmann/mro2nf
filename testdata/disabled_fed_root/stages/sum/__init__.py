def main(args, outs):
    outs.total = sum(v for v in (args.what or {}).values() if v is not None)
