import time

import martian

_HOLD_BYTES = 3 << 28  # 0.75 GB: above the 0.5 GB request, below the 2 GB retry


def main(args, outs):
    # pylint: disable=unused-argument
    held = bytearray(_HOLD_BYTES)
    for i in range(0, len(held), 4096):
        held[i] = 1
    time.sleep(2)
    outs.mem_gb = martian.get_memory_allocation()
    outs.arg_mem_gb = args.__mem_gb
