// Command mro2nf transpiles a Martian (.mro) pipeline into a Nextflow project.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/eunmann/mro2nf/internal/config"
	"github.com/eunmann/mro2nf/internal/emit"
	"github.com/eunmann/mro2nf/internal/frontend"
	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/logging"
	"github.com/eunmann/mro2nf/internal/overrides"
	"github.com/rs/zerolog"
)

// version is set via -ldflags at build time.
var version = "dev"

// errUsage is returned when the command-line arguments are invalid.
var errUsage = errors.New("usage: mro2nf [-o dir] [-mropath path] <pipeline.mro>")

// errFlagConflict aborts a transpile when a diagnostic reports an enabled flag
// would produce a wrong or broken project for the pipeline.
var errFlagConflict = errors.New("aborted on a flag/pipeline conflict (see error diagnostics above)")

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
	fuseChainsFlag := fs.Bool("fuse-chains", false, "fuse a single-consumer equal-resource source stage into its consumer's task, dropping a node (coarsens -resume; #59 Lever 4)")
	foldDisablesFlag := fs.Bool("fold-disables", false, "constant-fold an entry-determinable disable branch: an always-disabled stage is pruned (asserts you will not override its gate input; #59 Lever 1)")
	nativeFlag := fs.Bool("native", false, "opt-in channel-native orchestration (#76 M1): bake entry args, no BUILD_ENTRY_ARGS task (entry inputs fixed at transpile; no launch override)")
	nativeRunnerFlag := fs.Bool("native-runner", false, "opt-in direct-call Python stage runner (#79): no martian_shell.py adapter or mre broker on the stage hop (py stages only; the runner is baked into the image for container backends)")
	configFlag := fs.String("config", "", "path to .mro2nf.yml (default: alongside the .mro); its keys set flag defaults, explicit flags win")
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

	if err := applyConfig(fs, *configFlag, fs.Arg(0), cliPtrs{
		target: targetFlag, container: containerFlag, mre: mreFlag, shell: shellFlag,
		mrjob: mrjobFlag, monitor: monitorFlag, fuseChains: fuseChainsFlag, foldDisables: foldDisablesFlag,
	}); err != nil {
		return fmt.Errorf("transpile %s: %w", fs.Arg(0), err)
	}

	log := logging.New()

	prog, err := loadProgram(fs.Arg(0), *mroPath)
	if err != nil {
		return err
	}

	target, err := emit.ParseTarget(*targetFlag)
	if err != nil {
		return fmt.Errorf("invalid -target: %w", err)
	}

	if err := reportDiagnostics(log, prog, diagOpts{fuseChains: *fuseChainsFlag, foldDisables: *foldDisablesFlag, native: *nativeFlag, nativeRunner: *nativeRunnerFlag, monitor: *monitorFlag, target: target}); err != nil {
		return fmt.Errorf("transpile %s: %w", fs.Arg(0), err)
	}

	if err := emitProgram(prog, fs.Arg(0), opts{
		outDir: *outDir, mre: *mreFlag, shell: *shellFlag, mrjob: *mrjobFlag,
		container: *containerFlag, monitor: *monitorFlag, target: target,
		fuseChains: *fuseChainsFlag, foldDisables: *foldDisablesFlag, native: *nativeFlag,
		nativeRunner: *nativeRunnerFlag,
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
// with no faithful Nextflow directive are reported on stderr. With -mro, the
// pipeline is parsed so pipeline-scoped keys expand to their stages, unknown
// keys are reported instead of silently emitting a dead selector, and per-call
// selectors carry literal pipeline names — without it, a call name containing
// "__" can be over-matched by another call's selector (see internal/overrides).
func runOverrides(args []string) error {
	fs := flag.NewFlagSet("overrides", flag.ContinueOnError)
	mroFlag := fs.String("mro", "", "pipeline .mro: expands pipeline-scoped keys to their stages, validates stage names, "+
		"and pins per-call selectors to literal pipeline names (avoids over-matching call names containing '__')")
	mroPath := fs.String("mropath", ".", "search path for @include (os.PathListSeparator-separated)")

	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	prog, err := loadOverrideProgram(*mroFlag, *mroPath)
	if err != nil {
		return err
	}

	raw, err := readOverridesInput(fs.Arg(0))
	if err != nil {
		return err
	}

	cfg, unmapped, err := overrides.Convert(raw, prog)
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

// loadProgram parses and lowers the pipeline at src into the transpiler IR.
func loadProgram(src, mroPath string) (*ir.Program, error) {
	ast, err := frontend.Parse(src, filepath.SplitList(mroPath), false)
	if err != nil {
		return nil, fmt.Errorf("transpile %s: %w", src, err)
	}

	prog, err := frontend.Lower(ast)
	if err != nil {
		return nil, fmt.Errorf("transpile %s: %w", src, err)
	}

	return prog, nil
}

// loadOverrideProgram parses the pipeline at mro (may be empty -> nil program)
// so the converter can resolve pipeline-scoped keys.
func loadOverrideProgram(mro, mroPath string) (*ir.Program, error) {
	if mro == "" {
		return nil, nil
	}

	ast, err := frontend.Parse(mro, filepath.SplitList(mroPath), false)
	if err != nil {
		return nil, fmt.Errorf("parse pipeline %s: %w", mro, err)
	}

	prog, err := frontend.Lower(ast)
	if err != nil {
		return nil, fmt.Errorf("lower pipeline %s: %w", mro, err)
	}

	return prog, nil
}

// readOverridesInput reads the overrides JSON from arg (a file), or stdin when
// arg is empty or "-".
func readOverridesInput(arg string) ([]byte, error) {
	if arg == "" || arg == "-" {
		raw, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("read overrides from stdin: %w", err)
		}

		return raw, nil
	}

	raw, err := os.ReadFile(arg)
	if err != nil {
		return nil, fmt.Errorf("read overrides %s: %w", arg, err)
	}

	return raw, nil
}

// reportDiagnostics runs the pre-emit checks, prints each by severity, and
// returns an error (aborting the transpile) if any is fatal — an enabled flag
// that would produce a wrong or broken project for this pipeline.
// diagOpts carries the flag subset Diagnose needs; it must mirror what
// emitProgram passes to Emit or the diagnostics analyze a different plan.
type diagOpts struct {
	fuseChains, foldDisables, native, nativeRunner, monitor bool
	target                                                  emit.Target
}

func reportDiagnostics(log zerolog.Logger, prog *ir.Program, o diagOpts) error {
	diags := emit.Diagnose(prog, emit.Options{
		FuseChains: o.fuseChains, FoldDisables: o.foldDisables,
		Native: o.native, NativeRunner: o.nativeRunner, Monitor: o.monitor,
		Target: o.target,
	})

	for _, d := range diags {
		switch d.Severity {
		case emit.SevError:
			log.Error().Msg(d.Message)
		case emit.SevWarn:
			log.Warn().Msg(d.Message)
		default:
			log.Info().Msg(d.Message)
		}
	}

	if emit.HasError(diags) {
		return errFlagConflict
	}

	return nil
}

// cliPtrs collects the transpile flag value pointers a .mro2nf.yml may default.
type cliPtrs struct {
	target, container, mre, shell, mrjob *string
	monitor, fuseChains, foldDisables    *bool
}

// applyConfig loads the .mro2nf.yml (explicit path, else alongside the .mro) and
// sets each flag the user did NOT pass explicitly to the config's value —
// precedence is builtin default < config file < explicit flag. An explicit
// -config path must exist — a typo there must not silently drop the defaults —
// while the implicit alongside-the-.mro probe tolerates a missing file. An
// empty -config value is indistinguishable from an unset flag, so it takes the
// implicit probe.
func applyConfig(fs *flag.FlagSet, explicit, mroPath string, p cliPtrs) error {
	var (
		cfg *config.Config
		err error
	)

	if explicit != "" {
		cfg, err = config.LoadRequired(explicit)
	} else {
		cfg, err = config.Load(filepath.Join(filepath.Dir(mroPath), config.FileName))
	}

	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	applyStr := func(name string, dst, cv *string) {
		if !set[name] && cv != nil {
			*dst = *cv
		}
	}

	applyBool := func(name string, dst, cv *bool) {
		if !set[name] && cv != nil {
			*dst = *cv
		}
	}

	applyStr("target", p.target, cfg.Target)
	applyStr("container", p.container, cfg.Container)
	applyStr("mre", p.mre, cfg.Mre)
	applyStr("shell", p.shell, cfg.Shell)
	applyStr("mrjob", p.mrjob, cfg.Mrjob)
	applyBool("monitor", p.monitor, cfg.Monitor)
	applyBool("fuse-chains", p.fuseChains, cfg.FuseChains)
	applyBool("fold-disables", p.foldDisables, cfg.FoldDisables)

	return nil
}

// opts groups the CLI options that shape emission.
type opts struct {
	outDir, mre, shell, mrjob, container string
	monitor                              bool
	fuseChains                           bool
	foldDisables                         bool
	native                               bool
	nativeRunner                         bool
	target                               emit.Target
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
		OutDir:       o.outDir,
		Mre:          absOrSelf(o.mre),
		Shell:        absOrSelf(o.shell),
		Mrjob:        absOrSelf(o.mrjob),
		Container:    o.container,
		Monitor:      o.monitor,
		FuseChains:   o.fuseChains,
		FoldDisables: o.foldDisables,
		Native:       o.native,
		NativeRunner: o.nativeRunner,
		Target:       o.target,
		MROFile:      filepath.Base(src),
		MRODir:       mroDir,
		StageCode:    code,
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
