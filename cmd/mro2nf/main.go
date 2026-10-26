// Command mro2nf transpiles a Martian (.mro) pipeline into a Nextflow project.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"

	"github.com/eunmann/mro2nf/internal/buildinfo"
	"github.com/eunmann/mro2nf/internal/cgnf"
)

// version is set via -ldflags at build time.
var version = "dev"

var errUsage = errors.New("usage: mro2nf [-o dir] [-mropath path] <pipeline.mro>")

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "mro2nf:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("mro2nf", flag.ContinueOnError)
	f := defineTranspileFlags(fs)

	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	if *f.showVersion {
		// Writing the version line to stdout cannot meaningfully fail.
		_, _ = fmt.Fprintln(os.Stdout, "mro2nf", buildinfo.Version(version))

		return nil
	}

	if fs.NArg() != 1 {
		return errUsage
	}

	src := fs.Arg(0)

	opts, err := f.options(src)
	if err != nil {
		return fmt.Errorf("transpile %s: %w", src, err)
	}

	if err := cgnf.Emit(src, opts); err != nil {
		return fmt.Errorf("transpile %s: %w", src, err)
	}

	slog.New(slog.NewTextHandler(os.Stderr, nil)).Info("emitted Nextflow project", "source", src, "out", opts.OutDir)

	return nil
}

// transpileFlags collects every transpile flag in one struct, so run() reads
// parsed values from a single place and cgnf.Options is derived rather than
// hand-copied.
type transpileFlags struct {
	outDir, mroPath *string
	showVersion     *bool
}

// defineTranspileFlags registers the transpile flags on fs and returns their
// value pointers.
func defineTranspileFlags(fs *flag.FlagSet) transpileFlags {
	return transpileFlags{
		outDir:      fs.String("o", "out", "output directory for the generated Nextflow project"),
		mroPath:     fs.String("mropath", "", mroPathUsage),
		showVersion: fs.Bool("version", false, "print version and exit"),
	}
}

// options builds the cgnf.Options of a transpile of src: the include paths
// are the search path's roots.
func (f transpileFlags) options(src string) (cgnf.Options, error) {
	paths, err := mroPaths(searchPath(*f.mroPath, src))
	if err != nil {
		return cgnf.Options{}, err
	}

	return cgnf.Options{
		OutDir:   *f.outDir,
		MroPaths: paths,
		Version:  buildinfo.Version(version),
	}, nil
}

const mroPathUsage = "search path for @include (os.PathListSeparator-separated; default $MROPATH, else the .mro's directory, as mrp)"

// searchPath is the @include search path for the pipeline at src: given, else
// $MROPATH, else src's directory — mrp's rule (martian cmd/mrp/configure.go,
// "Compute MRO path").
func searchPath(given, src string) []string {
	if given != "" {
		return filepath.SplitList(given)
	}

	if env := os.Getenv("MROPATH"); env != "" {
		return filepath.SplitList(env)
	}

	return []string{filepath.Dir(src)}
}

// mroPaths are the search path's roots, absolute, each once. The entry
// file's dir is no root unless the search path names it: Martian searches
// it only for the entry's own includes and stage code, as it compiles the
// entry there.
func mroPaths(search []string) ([]string, error) {
	var out []string

	for _, p := range search {
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, fmt.Errorf("resolve -mropath %s: %w", p, err)
		}

		if !slices.Contains(out, abs) {
			out = append(out, abs)
		}
	}

	return out, nil
}
