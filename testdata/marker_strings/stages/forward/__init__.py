import os


def main(args, outs):
    outs.made = args.made
    outs.fake = args.fake
    outs.s = args.s
    # Records what the file-typed `fake` input looked like to the stage: mrp
    # hands it the literal string, which names no file.
    outs.seen = "%s exists=%s" % (args.fake, os.path.exists(args.fake))
