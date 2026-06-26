// Command mart transpiles a Martian (.mro) pipeline into a Nextflow project.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/eunmann/martian-nextflow/internal/emit"
	"github.com/eunmann/martian-nextflow/internal/frontend"
	"github.com/eunmann/martian-nextflow/internal/ir"
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
	mreFlag := fs.String("mre", "mre", "path to the mre shim binary")
	shellFlag := fs.String("shell", "", "path to martian_shell.py")
	mrjobFlag := fs.String("mrjob", "", "path to mrjob (for comp stages)")
	containerFlag := fs.String("container", "", "container image for processes (e.g. an ECR URI for cloud backends)")
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

	if err := emitProgram(prog, fs.Arg(0), opts{
		outDir: *outDir, mre: *mreFlag, shell: *shellFlag, mrjob: *mrjobFlag, container: *containerFlag,
	}); err != nil {
		return fmt.Errorf("transpile %s: %w", fs.Arg(0), err)
	}

	log.Info().
		Str("source", fs.Arg(0)).
		Int("stages", len(prog.Stages)).
		Int("pipelines", len(prog.Pipelines)).
		Str("out", *outDir).
		Msg("emitted Nextflow project")

	return nil
}

// opts groups the CLI options that shape emission.
type opts struct {
	outDir, mre, shell, mrjob, container string
}

// emitProgram resolves the absolute paths the generated project needs and emits
// the Nextflow project for prog.
func emitProgram(prog *ir.Program, src string, o opts) error {
	mroDir := filepath.Dir(src)

	code := make(map[string]string, len(prog.Stages))

	for name, s := range prog.Stages {
		path := s.SrcPath
		if !filepath.IsAbs(path) {
			path = filepath.Join(mroDir, path)
		}

		abs, err := filepath.Abs(path)
		if err != nil {
			return fmt.Errorf("resolve stage %s src: %w", name, err)
		}

		code[name] = abs
	}

	if err := emit.Emit(prog, emit.Options{
		OutDir:    o.outDir,
		Mre:       absOrSelf(o.mre),
		Shell:     absOrSelf(o.shell),
		Mrjob:     absOrSelf(o.mrjob),
		Container: o.container,
		MROFile:   filepath.Base(src),
		StageCode: code,
	}); err != nil {
		return fmt.Errorf("emit: %w", err)
	}

	return nil
}

// absOrSelf returns the absolute form of a path, falling back to the original
// (e.g. a bare command name like "mre" found on PATH).
func absOrSelf(p string) string {
	if p == "" || p == "mre" {
		return p
	}

	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}

	return p
}
