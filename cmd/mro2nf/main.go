// Command mro2nf transpiles a Martian (.mro) pipeline into a Nextflow project.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

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

// errOverridesUsage is returned when the overrides subcommand receives extra
// positional arguments (it takes at most one overrides file).
var errOverridesUsage = errors.New("usage: mro2nf overrides [-mro pipeline.mro] [-mropath path] [overrides.json]")

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
	f := defineTranspileFlags(fs)

	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	if *f.showVersion {
		// Writing the version line to stdout cannot meaningfully fail.
		_, _ = fmt.Fprintln(os.Stdout, "mro2nf", version)

		return nil
	}

	if fs.NArg() != 1 {
		return errUsage
	}

	src := fs.Arg(0)
	if err := applyConfig(fs, *f.configPath, src, f.configPtrs()); err != nil {
		return fmt.Errorf("transpile %s: %w", src, err)
	}

	log := logging.New()

	prog, err := loadProgram(src, *f.mroPath)
	if err != nil {
		return err
	}

	opts, err := f.options()
	if err != nil {
		return err
	}

	// One Options value feeds both Diagnose and Emit, so the diagnostics always
	// analyze exactly the plan that is emitted.
	if err := reportDiagnostics(log, prog, opts); err != nil {
		return fmt.Errorf("transpile %s: %w", src, err)
	}

	if err := emitProgram(prog, src, opts); err != nil {
		return fmt.Errorf("transpile %s: %w", src, err)
	}

	log.Info().
		Str("source", src).
		Int("stages", len(prog.Stages)).
		Int("pipelines", len(prog.Pipelines)).
		Str("out", opts.OutDir).
		Msg("emitted Nextflow project")

	return nil
}

// transpileFlags collects every flag of the default (transpile) command in one
// struct, so run() reads parsed values from a single place and the config
// pointers and emit.Options are derived rather than hand-copied.
type transpileFlags struct {
	outDir, mroPath, mre, shell, mrjob, container, target, configPath *string
	monitor, fuseChains, foldDisables, native, nativeRunner           *bool
	inlinePipelines                                                   *bool
	showVersion                                                       *bool
}

// defineTranspileFlags registers the transpile flags on fs and returns their
// value pointers.
func defineTranspileFlags(fs *flag.FlagSet) transpileFlags {
	return transpileFlags{
		outDir:          fs.String("o", "out", "output directory for the generated Nextflow project"),
		mroPath:         fs.String("mropath", ".", "search path for @include (os.PathListSeparator-separated)"),
		mre:             fs.String("mre", "mre", "path to the mre shim binary"),
		shell:           fs.String("shell", "", "path to martian_shell.py"),
		mrjob:           fs.String("mrjob", "", "path to mrjob (for comp stages)"),
		container:       fs.String("container", "", "container image for processes (e.g. an ECR URI for cloud backends)"),
		target:          fs.String("target", "local", "execution backend: local | awsbatch | healthomics"),
		monitor:         fs.Bool("monitor", false, "enforce mrp --monitor memory limits per stage: an RSS kill at mem_gb plus a prlimit vmem cap at vmem_gb"),
		fuseChains:      fs.Bool("fuse-chains", false, "fuse a single-consumer equal-resource source stage into its consumer's task, dropping a node (coarsens -resume; #59 Lever 4)"),
		foldDisables:    fs.Bool("fold-disables", false, "constant-fold an entry-determinable disable branch: an always-disabled stage is pruned (asserts you will not override its gate input; #59 Lever 1)"),
		native:          fs.Bool("native", false, "opt-in channel-native orchestration (#76 M1): bake entry args, no BUILD_ENTRY_ARGS task (entry inputs fixed at transpile; no launch override)"),
		nativeRunner:    fs.Bool("native-runner", false, "opt-in direct-call Python stage runner (#79): no martian_shell.py adapter or mre broker on the stage hop (py stages only; the runner is baked into the image for container backends)"),
		inlinePipelines: fs.Bool("inline-pipelines", false, "flatten eligible sub-pipeline boundaries into their parent, dropping the entry/return BIND tasks (orchestration-only, byte-identical outputs; coarsens -resume; #221)"),
		configPath:      fs.String("config", "", "path to .mro2nf.yml (default: alongside the .mro); its keys set flag defaults, explicit flags win (bools need the equals form, e.g. -native=false)"),
		showVersion:     fs.Bool("version", false, "print version and exit"),
	}
}

// configPtrs returns the flag value pointers a .mro2nf.yml may default.
func (f transpileFlags) configPtrs() cliPtrs {
	return cliPtrs{
		target: f.target, container: f.container, mre: f.mre, shell: f.shell,
		mrjob: f.mrjob, monitor: f.monitor, fuseChains: f.fuseChains, foldDisables: f.foldDisables,
		native: f.native, nativeRunner: f.nativeRunner,
	}
}

// options builds the single emit.Options both Diagnose and Emit consume — a new
// flag added here reaches both, so they cannot diverge. emitProgram fills in
// the source-derived fields (absolute tool paths, MROFile/MRODir, StageCode).
func (f transpileFlags) options() (emit.Options, error) {
	target, err := emit.ParseTarget(*f.target)
	if err != nil {
		return emit.Options{}, fmt.Errorf("invalid -target: %w", err)
	}

	return emit.Options{
		OutDir: *f.outDir, Mre: *f.mre, Shell: *f.shell, Mrjob: *f.mrjob,
		Container: *f.container, Monitor: *f.monitor, Target: target,
		FuseChains: *f.fuseChains, FoldDisables: *f.foldDisables,
		Native: *f.native, NativeRunner: *f.nativeRunner,
		InlinePipelines: *f.inlinePipelines,
	}, nil
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

	if fs.NArg() > 1 {
		return errOverridesUsage
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

	prog, err := frontend.Lower(ast, filepath.SplitList(mroPath))
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

	prog, err := frontend.Lower(ast, filepath.SplitList(mroPath))
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
// that would produce a wrong or broken project for this pipeline. It receives
// the same Options that emitProgram passes to Emit, so Diagnose analyzes the
// plan that is actually emitted.
func reportDiagnostics(log zerolog.Logger, prog *ir.Program, opts emit.Options) error {
	diags := emit.Diagnose(prog, opts)

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
	native, nativeRunner                 *bool
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
	applyBool("native", p.native, cfg.Native)
	applyBool("native-runner", p.nativeRunner, cfg.NativeRunner)

	return nil
}

// emitProgram fills in the source-derived Options fields (absolute tool paths,
// stageCodePaths maps each stage to its absolute stage-code path. SrcPath is
// already resolved against the declaring file's directory by the frontend (an
// @included stage's src is relative to the included file, NOT the entry .mro), so
// this only absolutizes it against the process cwd — it must NOT re-join it
// against the entry .mro's directory (which would double-prefix an @included
// stage's already-resolved relative path).
func stageCodePaths(prog *ir.Program) (map[string]string, error) {
	code := make(map[string]string, len(prog.Stages))

	for name, s := range prog.Stages {
		// A comp/exec stage whose src is a bare command (e.g. CellRanger's `cr_lib`)
		// is resolved on PATH at exec time; absolutizing it would produce a broken
		// filesystem path. Keep it verbatim.
		if s.SrcIsPathCommand() {
			code[name] = s.SrcPath

			continue
		}

		abs, err := filepath.Abs(s.SrcPath)
		if err != nil {
			return nil, fmt.Errorf("resolve stage %s src: %w", name, err)
		}

		code[name] = abs
	}

	return code, nil
}

// MROFile/MRODir, StageCode) and emits the Nextflow project for prog.
func emitProgram(prog *ir.Program, src string, opts emit.Options) error {
	mroDir := filepath.Dir(src)

	code, err := stageCodePaths(prog)
	if err != nil {
		return err
	}

	opts.Mre = absOrSelf(opts.Mre)
	opts.Shell = absOrSelf(opts.Shell)
	opts.Mrjob = absOrSelf(opts.Mrjob)
	opts.MROFile = filepath.Base(src)
	opts.MRODir = mroDir
	opts.StageCode = code

	if err := emit.Emit(prog, opts); err != nil {
		return fmt.Errorf("emit: %w", err)
	}

	return nil
}

// absOrSelf resolves a tool flag the way exec.LookPath dispatches: a bare
// command name (no path separator) is left as-is for PATH lookup at task run
// time, while any path containing a separator is absolutized so the generated
// project does not depend on Nextflow's per-task working directory.
func absOrSelf(p string) string {
	if !strings.ContainsRune(p, filepath.Separator) {
		return p
	}

	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}

	return p
}
