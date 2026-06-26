import martian

__MRO__ = """
stage PROBE(
    in  int   x,
    out float mem,
    out float threads,
)
"""


def main(args, outs):
    martian.update_progress("probing")
    martian.log_warn("a benign warning")
    martian.alarm("a benign alarm")
    outs.mem = martian.get_memory_allocation()
    outs.threads = martian.get_threads_allocation()
