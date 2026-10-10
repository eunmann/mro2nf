def main(args, outs):
    arr = [sorted((k, v) for k, v in m.items() if k != "report") for m in args.made]
    mp = {k: sorted((f, v) for f, v in m.items() if f != "report") for k, m in args.madem.items()}
    outs.r = repr([arr, sorted(mp.items()), [type(m).__name__ for m in args.made]])
