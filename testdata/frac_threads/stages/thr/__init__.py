import martian


def main(args, outs):
    outs.t = martian.get_threads_allocation()
    outs.at = args.__threads
