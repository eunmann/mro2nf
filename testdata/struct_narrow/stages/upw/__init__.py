import os


def main(args, outs):
    path = os.path.join(os.getcwd(), "w.txt")
    with open(path, "w") as f:
        f.write("w\n")
    outs.w = {"a": 4, "b": 5, "c": 6, "f": path}
