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
	"strings"

	"github.com/eunmann/mro2nf/internal/cgnf"
	"github.com/eunmann/mro2nf/internal/config"
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

var (
	errUsage   = errors.New("usage: mro2nf [-o dir] [-mropath path] <pipeline.mro>")
	errNoShell = errors.New("-shell is required: Martian's python adapter (<adapters>/python/martian_shell.py)")
)

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

	if err := applyConfig(fs, *f.configPath, src, f.configPtrs()); err != nil {
		return fmt.Errorf("transpile %s: %w", src, err)
	}

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
// parsed values from a single place and the config pointers and cgnf.Options
// are derived rather than hand-copied.
type transpileFlags struct {
	outDir, mroPath, mre, shell, mrjob, configPath *string
	monitor, confineOutputs, showVersion           *bool
}

// defineTranspileFlags registers the transpile flags on fs and returns their
// value pointers.
func defineTranspileFlags(fs *flag.FlagSet) transpileFlags {
	return transpileFlags{
		outDir:         fs.String("o", "out", "output directory for the generated Nextflow project"),
		mroPath:        fs.String("mropath", "", mroPathUsage),
		mre:            fs.String("mre", "mre", "path to the mre shim binary"),
		shell:          fs.String("shell", "", "path to martian_shell.py in Martian's adapters dir (<adapters>/python/martian_shell.py)"),
		mrjob:          fs.String("mrjob", "", "path to mrjob, which runs the py and comp stages (default: mrjob beside -mre)"),
		monitor:        fs.Bool("monitor", false, "mrp's --monitor: mrjob enforces each job's memory and thread limits"),
		confineOutputs: fs.Bool("confine-outputs", false, "fail a stage task whose output resolves outside its work dir instead of copying it in (set it for a project run under an isolating executor such as docker)"),
		configPath:     fs.String("config", "", "path to .mro2nf.yml (default: alongside the .mro); its keys set flag defaults, explicit flags win (bools need the equals form, e.g. -monitor=false)"),
		showVersion:    fs.Bool("version", false, "print version and exit"),
	}
}

// configPtrs returns the flag value pointers a .mro2nf.yml may default.
func (f transpileFlags) configPtrs() cliPtrs {
	return cliPtrs{mre: f.mre, shell: f.shell, mrjob: f.mrjob, monitor: f.monitor}
}

// options builds the cgnf.Options of a transpile of src: the tools made
// absolute (resolveTools), mrjob defaulting to the one beside mre, Martian's
// adapters dir the one holding -shell's python/, and the include paths the
// search path's roots then src's dir, where Martian resolves the entry's own
// includes and stage code.
func (f transpileFlags) options(src string) (cgnf.Options, error) {
	mre, shell, mrjob := *f.mre, *f.shell, *f.mrjob

	if shell == "" {
		return cgnf.Options{}, errNoShell
	}

	if err := resolveTools(&mre, &shell, &mrjob, ""); err != nil {
		return cgnf.Options{}, err
	}

	if mrjob == "" {
		mrjob = filepath.Join(filepath.Dir(mre), "mrjob")
	}

	paths, err := mroPaths(src, searchPath(*f.mroPath, src))
	if err != nil {
		return cgnf.Options{}, err
	}

	return cgnf.Options{
		OutDir:   *f.outDir,
		Mre:      mre,
		Mrjob:    mrjob,
		Adapters: filepath.Dir(filepath.Dir(shell)),
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

// mroPaths are the search path's roots, absolute, then the entry file's dir,
// each once.
func mroPaths(src string, search []string) ([]string, error) {
	var out []string

	for _, p := range append(slices.Clone(search), filepath.Dir(src)) {
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

// cliPtrs collects the transpile flag value pointers a .mro2nf.yml may default.
type cliPtrs struct {
	mre, shell, mrjob *string
	monitor           *bool
}

// applyConfig loads the .mro2nf.yml (explicit path, else alongside the .mro) and
// sets each flag the user did NOT pass explicitly to the config's value —
// precedence is builtin default < config file < explicit flag. An explicit
// -config path must exist — a typo there must not silently drop the defaults —
// while the implicit alongside-the-.mro probe tolerates a missing file. An
// empty -config value is indistinguishable from an unset flag, so it takes the
// implicit probe. A relative tool path in the file is relative to the file's
// directory, so a checked-in config names the same tools from any working
// directory.
func applyConfig(fs *flag.FlagSet, explicit, mroFile string, p cliPtrs) error {
	path, load := explicit, config.LoadRequired
	if path == "" {
		path, load = filepath.Join(filepath.Dir(mroFile), config.FileName), config.Load
	}

	cfg, err := load(path)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	if err := resolveTools(cfg.Mre, cfg.Shell, cfg.Mrjob, filepath.Dir(path)); err != nil {
		return fmt.Errorf("config %s: %w", path, err)
	}

	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	applyStr := func(name string, dst, cv *string) {
		if !set[name] && cv != nil {
			*dst = *cv
		}
	}

	applyStr("mre", p.mre, cfg.Mre)
	applyStr("shell", p.shell, cfg.Shell)
	applyStr("mrjob", p.mrjob, cfg.Mrjob)

	if !set["monitor"] && cfg.Monitor != nil {
		*p.monitor = *cfg.Monitor
	}

	return nil
}

// resolveTools makes the mre, shell and mrjob paths the project bakes in
// absolute in place, a relative one taken against base, so the project does
// not depend on Nextflow's per-task working directory. mre and mrjob are
// exec'd, so a bare name stays a PATH command; shell names the adapters dir,
// so even a bare name is a file.
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
