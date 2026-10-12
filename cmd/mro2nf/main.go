// Command mro2nf transpiles a Martian (.mro) pipeline into a Nextflow project.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/eunmann/mro2nf/internal/config"
	"github.com/eunmann/mro2nf/internal/emit"
	"github.com/eunmann/mro2nf/internal/frontend"
	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/logging"
	"github.com/eunmann/mro2nf/internal/overrides"
)

// version is set via -ldflags at build time.
var version = "dev"

// buildVersion names this build in every task's -resume cache key
// (emit.Options.Version).
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
		_, _ = fmt.Fprintln(os.Stdout, "mro2nf", buildVersion())

		return nil
	}

	if fs.NArg() != 1 {
		return errUsage
	}

	src := fs.Arg(0)

	fromConfig, err := applyConfig(fs, *f.configPath, src, f.configPtrs())
	if err != nil {
		return fmt.Errorf("transpile %s: %w", src, err)
	}

	log := logging.New()

	opts, err := f.options(src, fromConfig)
	if err != nil {
		return err
	}

	prog, err := loadProgram(src, opts.MROPath)
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

	log.Info("emitted Nextflow project",
		"source", src,
		"stages", len(prog.Stages),
		"pipelines", len(prog.Pipelines),
		"out", opts.OutDir)

	return nil
}

// transpileFlags collects every flag of the default (transpile) command in one
// struct, so run() reads parsed values from a single place and the config
// pointers and emit.Options are derived rather than hand-copied.
type transpileFlags struct {
	outDir, mroPath, mre, shell, mrjob, container, target, configPath *string
	monitor, native                                                   *bool
	inlinePipelines, confineOutputs, allowExternalFiles               *bool
	showVersion                                                       *bool
}

// defineTranspileFlags registers the transpile flags on fs and returns their
// value pointers.
func defineTranspileFlags(fs *flag.FlagSet) transpileFlags {
	return transpileFlags{
		outDir:             fs.String("o", "out", "output directory for the generated Nextflow project"),
		mroPath:            fs.String("mropath", "", mroPathUsage),
		mre:                fs.String("mre", "mre", "path to the mre shim binary"),
		shell:              fs.String("shell", "", "path to martian_shell.py"),
		mrjob:              fs.String("mrjob", "", "path to mrjob (for comp stages)"),
		container:          fs.String("container", "", "container image for processes (e.g. an ECR URI for cloud backends)"),
		target:             fs.String("target", "local", "execution backend: local | awsbatch | healthomics"),
		monitor:            fs.Bool("monitor", false, "enforce mrp --monitor memory limits per stage: an RSS kill at mem_gb plus a prlimit vmem cap at vmem_gb"),
		native:             fs.Bool("native", false, "opt-in channel-native orchestration (#76 M1): bake entry args, no BUILD_ENTRY_ARGS task (entry inputs fixed at transpile; no launch override)"),
		inlinePipelines:    fs.Bool("inline-pipelines", true, "flatten eligible sub-pipeline boundaries into their parent, dropping the entry/return BIND tasks (ON by default; orchestration-only, byte-identical outputs; pass -inline-pipelines=false to keep per-boundary tasks for finer -resume; #221)"),
		confineOutputs:     fs.Bool("confine-outputs", false, "fail a stage task whose output resolves outside its work dir instead of copying it in (always on for awsbatch/healthomics; set it for a local project run under an isolating executor such as docker)"),
		allowExternalFiles: fs.Bool("allow-external-files", false, "copy entry file defaults and container-target stage code that resolve outside the .mro's directory and the -mropath roots into the project or image (refused by default)"),
		configPath:         fs.String("config", "", "path to .mro2nf.yml (default: alongside the .mro); its keys set flag defaults, explicit flags win (bools need the equals form, e.g. -native=false)"),
		showVersion:        fs.Bool("version", false, "print version and exit"),
	}
}

// configPtrs returns the flag value pointers a .mro2nf.yml may default.
func (f transpileFlags) configPtrs() cliPtrs {
	return cliPtrs{
		target: f.target, container: f.container, mre: f.mre, shell: f.shell,
		mrjob: f.mrjob, monitor: f.monitor, native: f.native,
	}
}

// options builds the single emit.Options both Diagnose and Emit consume — a new
// flag added here reaches both, so they cannot diverge. emitProgram fills in
// the source-derived fields (absolute tool paths, MROFile/MRODir, StageCode).
// The @include search path of the pipeline src also bounds the copied files:
// the user named it (-mropath or $MROPATH), or it is the .mro's directory,
// which bounds them anyway. fromConfig names the flags a config file set
// (applyConfig); the tools among them are not trusted.
func (f transpileFlags) options(src string, fromConfig map[string]bool) (emit.Options, error) {
	target, err := emit.ParseTarget(*f.target)
	if err != nil {
		return emit.Options{}, fmt.Errorf("invalid -target: %w", err)
	}

	tools := map[string]bool{}

	for _, name := range []string{"mre", "shell", "mrjob"} {
		if fromConfig[name] {
			tools[name] = true
		}
	}

	return emit.Options{
		OutDir: *f.outDir, Mre: *f.mre, Shell: *f.shell, Mrjob: *f.mrjob,
		Container: *f.container, Monitor: *f.monitor, Target: target,
		Native:          *f.native,
		InlinePipelines: *f.inlinePipelines, ConfineOutputs: *f.confineOutputs,
		MROPath: searchPath(*f.mroPath, src), AllowExternalFiles: *f.allowExternalFiles, ConfigTools: tools,
		Version: buildVersion(),
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
	mroPath := fs.String("mropath", "", mroPathUsage)

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

// loadProgram parses and lowers the pipeline at src into the transpiler IR.
func loadProgram(src string, mroPaths []string) (*ir.Program, error) {
	ast, err := frontend.Parse(src, mroPaths, false)
	if err != nil {
		return nil, fmt.Errorf("transpile %s: %w", src, err)
	}

	prog, err := frontend.Lower(ast, mroPaths)
	if err != nil {
		return nil, fmt.Errorf("transpile %s: %w", src, err)
	}

	if err := frontend.CheckCallGraph(ast); err != nil {
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

	mroPaths := searchPath(mroPath, mro)

	ast, err := frontend.Parse(mro, mroPaths, false)
	if err != nil {
		return nil, fmt.Errorf("parse pipeline %s: %w", mro, err)
	}

	prog, err := frontend.Lower(ast, mroPaths)
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
func reportDiagnostics(log *slog.Logger, prog *ir.Program, opts emit.Options) error {
	diags := emit.Diagnose(prog, opts)

	for _, d := range diags {
		switch d.Severity {
		case emit.SevError:
			log.Error(d.Message)
		case emit.SevWarn:
			log.Warn(d.Message)
		default:
			log.Info(d.Message)
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
	monitor, native                      *bool
}

// applyConfig loads the .mro2nf.yml (explicit path, else alongside the .mro) and
// sets each flag the user did NOT pass explicitly to the config's value —
// precedence is builtin default < config file < explicit flag. An explicit
// -config path must exist — a typo there must not silently drop the defaults —
// while the implicit alongside-the-.mro probe tolerates a missing file. An
// empty -config value is indistinguishable from an unset flag, so it takes the
// implicit probe. A relative tool path in the file is relative to the file's
// directory, so a checked-in config names the same tools from any working
// directory. It returns the names of the flags the config set.
func applyConfig(fs *flag.FlagSet, explicit, mroFile string, p cliPtrs) (map[string]bool, error) {
	path, load := explicit, config.LoadRequired
	if path == "" {
		path, load = filepath.Join(filepath.Dir(mroFile), config.FileName), config.Load
	}

	cfg, err := load(path)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}

	if err := resolveTools(cfg.Mre, cfg.Shell, cfg.Mrjob, filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}

	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	applied := map[string]bool{}

	applyStr := func(name string, dst, cv *string) {
		if !set[name] && cv != nil {
			*dst = *cv
			applied[name] = true
		}
	}

	applyBool := func(name string, dst, cv *bool) {
		if !set[name] && cv != nil {
			*dst = *cv
			applied[name] = true
		}
	}

	applyStr("target", p.target, cfg.Target)
	applyStr("container", p.container, cfg.Container)
	applyStr("mre", p.mre, cfg.Mre)
	applyStr("shell", p.shell, cfg.Shell)
	applyStr("mrjob", p.mrjob, cfg.Mrjob)
	applyBool("monitor", p.monitor, cfg.Monitor)
	applyBool("native", p.native, cfg.Native)

	return applied, nil
}

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

// emitProgram fills in the source-derived Options fields (absolute tool paths,
// MROFile/MRODir, StageCode) and emits the Nextflow project for prog.
func emitProgram(prog *ir.Program, src string, opts emit.Options) error {
	mroDir := filepath.Dir(src)

	code, err := stageCodePaths(prog)
	if err != nil {
		return err
	}

	if err := resolveToolPaths(&opts); err != nil {
		return err
	}

	opts.MROFile = filepath.Base(src)
	opts.MRODir = mroDir
	opts.StageCode = code

	if err := emit.Emit(prog, opts); err != nil {
		return fmt.Errorf("emit: %w", err)
	}

	return nil
}

// resolveToolPaths makes the -mre, -shell and -mrjob paths baked into the
// project absolute, so it does not depend on Nextflow's per-task working
// directory. -mre and -mrjob are exec'd, so a bare name stays a PATH command;
// -shell is the interpreter's script argument, which mre runs from the task's
// files dir, so even a bare name is a file.
func resolveToolPaths(opts *emit.Options) error {
	return resolveTools(&opts.Mre, &opts.Shell, &opts.Mrjob, "")
}

// resolveTools resolves the mre, shell and mrjob paths in place against base
// (see filePath) by the rules resolveToolPaths states.
func resolveTools(mre, shell, mrjob *string, base string) error {
	for _, tool := range []struct {
		name    string
		path    *string
		resolve func(p, base string) (string, error)
	}{
		{"mre", mre, commandPath}, {"shell", shell, filePath}, {"mrjob", mrjob, commandPath},
	} {
		if tool.path == nil {
			continue
		}

		resolved, err := tool.resolve(*tool.path, base)
		if err != nil {
			return fmt.Errorf("resolve %s: %w", tool.name, err)
		}

		*tool.path = resolved
	}

	return nil
}

// commandPath resolves an exec'd tool the way exec.LookPath dispatches: a
// bare name (no path separator) stays a PATH command, any other path is made
// absolute (see filePath).
func commandPath(p, base string) (string, error) {
	if !strings.ContainsRune(p, filepath.Separator) {
		return p, nil
	}

	return filePath(p, base)
}

// filePath makes a non-empty p absolute, a relative p taken relative to base,
// itself relative to the working directory.
func filePath(p, base string) (string, error) {
	if p == "" {
		return "", nil
	}

	if !filepath.IsAbs(p) {
		p = filepath.Join(base, p)
	}

	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("absolute path of %s: %w", p, err)
	}

	return abs, nil
}
