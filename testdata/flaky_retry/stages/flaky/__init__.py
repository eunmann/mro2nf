import martian


def main(args, outs):
    # pylint: disable=unused-argument
    mem = martian.get_memory_allocation()
    if mem <= 1:
        # An ordinary (non-ASSERT) failure: the adapter reports the traceback
        # on fd 4 without the ASSERT: prefix, so the shim exits 1 and Nextflow
        # retries with escalated memory.
        raise RuntimeError("transient failure below 2 GB (attempt 1)")
    outs.mem_gb = float(mem)
