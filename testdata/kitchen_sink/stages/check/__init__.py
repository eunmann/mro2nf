import martian


def main(args, outs):
    # pylint: disable=unused-argument
    if not args.values:
        martian.exit("no values provided")
    martian.log_info("preflight ok: %d values" % len(args.values))
