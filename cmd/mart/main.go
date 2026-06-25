// Command mart transpiles a Martian (.mro) pipeline into a Nextflow project.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/eunmann/martian-nextflow/internal/frontend"
	"github.com/eunmann/martian-nextflow/internal/logging"
)

// version is set via -ldflags at build time.
var version = "dev"

// errUsage is returned when the command-line arguments are invalid.
var errUsage = errors.New("usage: mart [-o dir] [-mropath path] <pipeline.mro>")

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "mart:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("mart", flag.ContinueOnError)
	outDir := fs.String("o", "out", "output directory for the generated Nextflow project")
	mroPath := fs.String("mropath", ".", "search path for @include (os.PathListSeparator-separated)")
	showVersion := fs.Bool("version", false, "print version and exit")

	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	if *showVersion {
		// Writing the version line to stdout cannot meaningfully fail.
		_, _ = fmt.Fprintln(os.Stdout, "mart", version)

		return nil
	}

	if fs.NArg() != 1 {
		return errUsage
	}

	log := logging.New()

	ast, err := frontend.Parse(fs.Arg(0), filepath.SplitList(*mroPath), false)
	if err != nil {
		return fmt.Errorf("transpile %s: %w", fs.Arg(0), err)
	}

	prog, err := frontend.Lower(ast)
	if err != nil {
		return fmt.Errorf("transpile %s: %w", fs.Arg(0), err)
	}

	entry := "(none)"
	if prog.Entry != nil {
		entry = prog.Entry.Callable
	}

	log.Info().
		Str("source", fs.Arg(0)).
		Int("stages", len(prog.Stages)).
		Int("pipelines", len(prog.Pipelines)).
		Str("entry", entry).
		Str("out", *outDir).
		Msg("lowered MRO to IR")

	return nil
}
