def main(args, outs):
    outs.xs = [1, 2, 3]
    outs.skips = [False, True, False]
    outs.entries = [{"x": 1, "skip": True}, {"x": 2, "skip": False}, {"x": 3, "skip": True}]
