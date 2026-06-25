// Command mre is the Martian runtime executor. It runs a single phase (split,
// main, or join) of a Martian stage against the original Martian adapter, for
// use inside a generated Nextflow process.
package main

import (
	"errors"
	"fmt"
	"os"
)

// version is set via -ldflags at build time.
var version = "dev"

// errNotImplemented marks phases not yet wired up.
var errNotImplemented = errors.New("not implemented")

// errUsage is returned when no phase argument is given.
var errUsage = errors.New("usage: mre <split|main|join|version> [flags]")

// errUnknownPhase is returned for an unrecognized phase argument.
var errUnknownPhase = errors.New("unknown phase")

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "mre:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errUsage
	}

	switch phase := args[0]; phase {
	case "version":
		// Writing the version line to stdout cannot meaningfully fail.
		_, _ = fmt.Fprintln(os.Stdout, "mre", version)

		return nil
	case "split", "main", "join":
		return fmt.Errorf("phase %q: %w", phase, errNotImplemented)
	default:
		return fmt.Errorf("%w: %q", errUnknownPhase, phase)
	}
}
