// Command mre is the Martian runtime executor the generated Nextflow tasks
// call: it compiles the run's call graph (plan), builds a stage instance's
// _args (args), runs one phase of the stage as mrp runs its job (run), and
// stamps and splits its outs (post); the OUTS task resolves and publishes the
// pipeline outs (outs).
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/eunmann/mro2nf/internal/job"
)

// version is set via -ldflags at build time.
var version = "dev"

var (
	errUsage          = errors.New("usage: mre <plan|args|run|post|outs|version> [flags]")
	errUnknownCommand = errors.New("unknown subcommand")
)

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "mre:", err)
		os.Exit(exitCode(err))
	}
}

// exitCode maps a stage failure to a process exit code: an ASSERT-class failure
// uses a distinct code so the generated Nextflow terminates rather than retries,
// and a job failure mrp retries another, the one the project retries.
func exitCode(err error) int {
	switch {
	case errors.Is(err, job.ErrAssert):
		return job.AssertExitCode
	case errors.Is(err, job.ErrTransient):
		return job.TransientExitCode
	default:
		return 1
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errUsage
	}

	if args[0] == "version" {
		// Writing the version line to stdout cannot meaningfully fail.
		_, _ = fmt.Fprintln(os.Stdout, "mre", version)

		return nil
	}

	cmd, ok := subcommands()[args[0]]
	if !ok {
		return fmt.Errorf("%w: %q", errUnknownCommand, args[0])
	}

	return cmd(ctx, args[1:])
}

func subcommands() map[string]func(context.Context, []string) error {
	return map[string]func(context.Context, []string) error{
		"plan": runPlan,
		"args": runArgs,
		"post": runPost,
		"outs": runOuts,
		"run":  runJob,
	}
}

// repeatedFlag collects every occurrence of a repeatable string flag, in order.
type repeatedFlag []string

func (r *repeatedFlag) String() string { return strings.Join(*r, " ") }

func (r *repeatedFlag) Set(v string) error {
	*r = append(*r, v)

	return nil
}
