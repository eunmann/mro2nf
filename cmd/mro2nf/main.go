// Command mro2nf transpiles a Martian (.mro) pipeline into a Nextflow project.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/eunmann/mro2nf/internal/emit"
	"github.com/eunmann/mro2nf/internal/frontend"
	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/logging"
	"github.com/eunmann/mro2nf/internal/overrides"
)

// version is set via -ldflags at build time.
var version = "dev"

// errUsage is returned when the command-line arguments are invalid.
var errUsage = errors.New("usage: mro2nf [-o dir] [-mropath path] <pipeline.mro>")

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "mro2nf:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	// `mro2nf overrides <file>` converts an mrp --overrides JSON to a Nextflow
	// -c config; the default (no subcommand) transpiles a pipeline.
	if len(args) > 0 && args[0] == "overrides" {
		return runOverrides(args[1:])
	}

	fs := flag.NewFlagSet("mro2nf", flag.ContinueOnError)
	outDir := fs.String("o", "out", "output directory for the generated Nextflow project")
	mroPath := fs.String("mropath", ".", "search path for @include (os.PathListSeparator-separated)")
	mreFlag := fs.String("mre", "mre", "path to the mre shim binary")
	shellFlag := fs.String("shell", "", "path to martian_shell.py")
	mrjobFlag := fs.String("mrjob", "", "path to mrjob (for comp stages)")
	containerFlag := fs.String("container", "", "container image for processes (e.g. an ECR URI for cloud backends)")
	targetFlag := fs.String("target", "local", "execution backend: local | awsbatch | healthomics")
	monitorFlag := fs.Bool("monitor", false, "enforce per-stage virtual memory (vmem_gb) via prlimit (mrp --monitor)")
	externalRuntimeFlag := fs.Bool("external-runtime", false, "container target: the image already provides mre/adapters/mrjob/stage code at the given paths; skip vendoring them and skip the Dockerfile")
	showVersion := fs.Bool("version", false, "print version and exit")

	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	if *showVersion {
		// Writing the version line to stdout cannot meaningfully fail.
		_, _ = fmt.Fprintln(os.Stdout, "mro2nf", version)

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

	target, err := emit.ParseTarget(*targetFlag)
	if err != nil {
		return fmt.Errorf("invalid -target: %w", err)
	}

	// Surface documented divergences (preflight/local/volatile no-ops) so a
	// ported pipeline's behavior differences are loud, not silent.
	for _, msg := range emit.Warnings(prog) {
		log.Warn().Msg(msg)
	}

	if err := emitProgram(prog, fs.Arg(0), opts{
		outDir: *outDir, mre: *mreFlag, shell: *shellFlag, mrjob: *mrjobFlag,
		container: *containerFlag, monitor: *monitorFlag, target: target,
		externalRuntime: *externalRuntimeFlag,
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

// runOverrides converts an mrp --overrides JSON (a file argument, or stdin when
// omitted or "-") into a Nextflow process-scope config printed to stdout. Fields
// with no faithful Nextflow directive are reported on stderr.
func runOverrides(args []string) error {
	var (
		raw []byte
		err error
	)

	if len(args) == 0 || args[0] == "-" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(args[0])
	}

	if err != nil {
		return fmt.Errorf("read overrides: %w", err)
	}

	cfg, unmapped, err := overrides.Convert(raw)
	if err != nil {
		return fmt.Errorf("convert overrides: %w", err)
	}

	for _, u := range unmapped {
		fmt.Fprintln(os.Stderr, "mro2nf overrides: skipped", u)
	}

	if _, err := fmt.Fprint(os.Stdout, cfg); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	return nil
}

// opts groups the CLI options that shape emission.
type opts struct {
	outDir, mre, shell, mrjob, container string
	monitor                              bool
	externalRuntime                      bool
	target                               emit.Target
}

// emitProgram resolves the absolute paths the generated project needs and emits
// the Nextflow project for prog.
func emitProgram(prog *ir.Program, src string, o opts) error {
	mroDir := filepath.Dir(src)

	code := make(map[string]string, len(prog.Stages))
	srcArgs := make(map[string][]string, len(prog.Stages))

	for name, s := range prog.Stages {
		code[name] = resolveStageCode(s.SrcPath, mroDir)

		if len(s.SrcArgs) > 0 {
			srcArgs[name] = s.SrcArgs
		}
	}

	if err := emit.Emit(prog, emit.Options{
		OutDir:          o.outDir,
		Mre:             absOrSelf(o.mre),
		Shell:           absOrSelf(o.shell),
		Mrjob:           absOrSelf(o.mrjob),
		Container:       o.container,
		Monitor:         o.monitor,
		ExternalRuntime: o.externalRuntime,
		Target:          o.target,
		MROFile:         filepath.Base(src),
		MRODir:          mroDir,
		StageCode:       code,
		StageArgs:       srcArgs,
	}); err != nil {
		return fmt.Errorf("emit: %w", err)
	}

	return nil
}

// resolveStageCode turns a stage's `src` path into the absolute path baked as
// -stagecode, resolving a relative path against the MRO dir. A comp stage names
// its executable (e.g. `cr_lib`, dispatched by its `martian <stage>` src args);
// it is resolved to an absolute path too, NOT left bare — a compiled Martian
// adapter derives its own location via current_exe/argv[0] (see
// martian_stages!), which fails for a bare name with no path. Deployments that
// invoke a bare binary via a shared filesystem place a resolvable file (often a
// symlink to the real binary) at <mroDir>/<name> so this path exists at run time.
func resolveStageCode(srcPath, mroDir string) string {
	path := srcPath
	if !filepath.IsAbs(path) {
		path = filepath.Join(mroDir, path)
	}

	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}

	return path
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
