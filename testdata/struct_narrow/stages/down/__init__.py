def main(args, outs):
    def keys(v):
        return sorted(v.items())
    outs.z = repr([keys(args.s), keys(args.s2), [keys(e) for e in args.ss],
                   sorted((k, keys(v)) for k, v in args.sm.items())])
