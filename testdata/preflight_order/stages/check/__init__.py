import martian


def main(args, outs):
    # pylint: disable=unused-argument
    if args.x < 0:
        martian.exit("x must not be negative, got %d" % args.x)
