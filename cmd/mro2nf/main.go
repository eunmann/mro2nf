// Command mro2nf transpiles a Martian (.mro) pipeline into a Nextflow project.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"

	"github.com/eunmann/mro2nf/internal/cgnf"
	"github.com/eunmann/mro2nf/internal/logging"
)

// version is set via -ldflags at build time.
var version = "dev"

// buildVersion names this build in every task's -resume cache key
// (cgnf.Options.Version).
func buildVersion() string {
	info, _ := debug.ReadBuildInfo()

	return chooseVersion(version, info)
}

// chooseVersion is the -ldflags stamp, else the main module version the Go
// toolchain stamps: the tag of a `go install …@v`, or for a plain `go build`
// in a clone a pseudo-version naming the commit (+dirty when modified), so
// unstamped builds of different commits do not share one "dev" key. Go
// stamps no version ("(devel)") under -buildvcs=false, outside a repository
// or in a git worktree, and those builds stay "dev".
func chooseVersion(stamp string, info *debug.BuildInfo) string {
	if stamp != "dev" {
		return stamp
	}

	if info != nil && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}

	return stamp
}

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
		_, _ = fmt.Fprintln(os.Stdout, "mro2nf", buildVersion())

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

	logging.New().Info("emitted Nextflow project", "source", src, "out", opts.OutDir)

	return nil
}

// transpileFlags collects every transpile flag in one struct, so run() reads
// parsed values from a single place and cgnf.Options is derived rather than
// hand-copied.
type transpileFlags struct {
	outDir, mroPath                      *string
	monitor, confineOutputs, showVersion *bool
}

// defineTranspileFlags registers the transpile flags on fs and returns their
// value pointers.
func defineTranspileFlags(fs *flag.FlagSet) transpileFlags {
	return transpileFlags{
		outDir:         fs.String("o", "out", "output directory for the generated Nextflow project"),
		mroPath:        fs.String("mropath", "", mroPathUsage),
		monitor:        fs.Bool("monitor", false, "mrp's --monitor: mrjob enforces each job's memory and thread limits"),
		confineOutputs: fs.Bool("confine-outputs", false, "fail a stage task whose output resolves outside its work dir instead of copying it in (set it for a project run under an isolating executor such as docker)"),
		showVersion:    fs.Bool("version", false, "print version and exit"),
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
		Monitor:  *f.monitor,
		Confine:  *f.confineOutputs,
		Version:  buildVersion(),
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
