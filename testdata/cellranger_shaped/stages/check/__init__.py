import martian


def main(args, outs):
    if args.gem_wells <= 0:
        martian.exit("gem_wells must be positive")
