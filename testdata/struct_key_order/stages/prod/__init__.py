import os


def main(args, outs):
    outs.s = {"z": 1, "a": 2}
    outs.w = {"a": 2, "m": 9, "z": 1}
    outs.bx = {"b": 3, "q": {"a": 2, "m": 9, "z": 1}}
    outs.fl = {"a": 2, "z": 1}
    outs.um = {"y": 1, "b": {"d": 1, "c": 2}}
    outs.z = 1
    outs.a = 2
    outs.m = 9
    path = os.path.join(os.getcwd(), "f.txt")
    with open(path, "w") as fh:
        fh.write("f\n")
    outs.fb = {"q": {"z": 1, "a": 2}, "f": path, "b": 3}
