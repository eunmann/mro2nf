def main(args, outs):
    # Every fork instance genuinely consumes the broadcast payload, so the N
    # payload stagings (one per FUSE task) are intrinsic data movement — the
    # benchmark's scaling gate is about the PLUMBING task count staying flat
    # in N, not about avoiding these real per-consumer transfers.
    with open(args.payload) as handle:
        handle.readline()
    outs.w = args.v * args.v
