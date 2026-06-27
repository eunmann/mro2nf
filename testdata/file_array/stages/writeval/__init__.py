import martian
def main(args, outs):
    p = martian.make_path("v.txt")
    if isinstance(p, bytes): p = p.decode("utf-8")
    with open(p, "w") as h: h.write(str(args.x))
    outs.f = p
