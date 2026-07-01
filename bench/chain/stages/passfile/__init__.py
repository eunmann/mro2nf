def main(args, outs):
    # Pass the input file through unchanged. Martian passes files by reference off a
    # shared filesystem, so this stage does no I/O; the transpiler's bundle transport
    # is what re-materializes the file into a fresh bundle at this hop. Emit-once
    # routing (#14) should make this a zero-transfer forward.
    outs.f = args.f
