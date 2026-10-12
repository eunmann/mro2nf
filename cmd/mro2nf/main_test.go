package main

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/config"
	"github.com/google/go-cmp/cmp"
)

const testShell = "/opt/adapters/python/martian_shell.py"

// runFixture runs mro2nf on a testdata fixture into a fresh dir and returns
// the project dir and the error.
func runFixture(t *testing.T, fixture string, flags ...string) (string, error) {
	t.Helper()

	out := filepath.Join(t.TempDir(), "proj")
	dir := filepath.Join("..", "..", "testdata", fixture)
	args := append([]string{"-o", out, "-mre", "/opt/bin/mre", "-shell", testShell, "-mropath", dir}, flags...)

	return out, run(append(args, filepath.Join(dir, "pipeline.mro")))
}

func readConfig(t *testing.T, proj string) string {
	t.Helper()

	cfg, err := os.ReadFile(filepath.Join(proj, "nextflow.config"))
	if err != nil {
		t.Fatal(err)
	}

	return string(cfg)
}

// TestRunEmitsCallGraphProject checks a transpile emits the call-graph
// project, its mrjob the one built beside mre and its adapters the shell's,
// confined under -confine-outputs.
func TestRunEmitsCallGraphProject(t *testing.T) {
	out, err := runFixture(t, "file_min")
	if err != nil {
		t.Fatal(err)
	}

	cfg := readConfig(t, out)
	for _, want := range []string{"params.mro2nf_mrjob = '/opt/bin/mrjob'", "params.mro2nf_adapters = '/opt/adapters'", "params.mro2nf_confine = false", "params.x = null"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("nextflow.config lacks %q:\n%s", want, cfg)
		}
	}

	if _, err := os.Stat(filepath.Join(out, "stage.nf")); err != nil {
		t.Errorf("no stage module: %v", err)
	}

	confined, err := runFixture(t, "file_min", "-confine-outputs")
	if err != nil {
		t.Fatal(err)
	}

	if cfg := readConfig(t, confined); !strings.Contains(cfg, "params.mro2nf_confine = true") {
		t.Errorf("-confine-outputs does not confine the stage tasks:\n%s", cfg)
	}
}

// TestUsageShowsDefaults checks the usage text lists each flag with its
// default.
func TestUsageShowsDefaults(t *testing.T) {
	fs := flag.NewFlagSet("mro2nf", flag.ContinueOnError)

	var usage bytes.Buffer

	fs.SetOutput(&usage)
	defineTranspileFlags(fs)
	fs.PrintDefaults()

	for _, want := range []string{"-mropath", "-confine-outputs", `(default "mre")`, `(default "out")`} {
		if !strings.Contains(usage.String(), want) {
			t.Errorf("usage lacks %s:\n%s", want, usage.String())
		}
	}
}

// TestRunRefusesBadInvocations checks a transpile fails, emitting no
// project, on an undefined flag, a missing .mro or one too many, an
// explicit -config that does not exist and a .mro that does not exist, and
// that -version transpiles nothing.
func TestRunRefusesBadInvocations(t *testing.T) {
	mro := filepath.Join("..", "..", "testdata", "file_min", "pipeline.mro")
	out := filepath.Join(t.TempDir(), "proj")

	for name, args := range map[string][]string{
		"flag":    {"-bogus", mro},
		"no mro":  {"-o", out},
		"two mro": {"-o", out, mro, mro},
		"config":  {"-o", out, "-shell", testShell, "-config", filepath.Join(t.TempDir(), "absent.yml"), mro},
		"mro":     {"-o", out, "-shell", testShell, filepath.Join(t.TempDir(), "absent.mro")},
	} {
		t.Run(name, func(t *testing.T) {
			if err := run(args); err == nil {
				t.Errorf("run(%v) succeeded", args)
			}
		})
	}

	if err := run([]string{"-version", "-o", out, mro}); err != nil {
		t.Errorf("-version: %v", err)
	}

	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused transpile left %s (stat: %v)", out, err)
	}
}

// TestRunRequiresShell checks a transpile without -shell, which names
// Martian's adapters dir, fails and emits no project.
func TestRunRequiresShell(t *testing.T) {
	out := filepath.Join(t.TempDir(), "proj")
	mro := filepath.Join("..", "..", "testdata", "file_min", "pipeline.mro")

	if err := run([]string{"-o", out, mro}); !errors.Is(err, errNoShell) {
		t.Errorf("no -shell: %v, want errNoShell", err)
	}

	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused transpile left %s (stat: %v)", out, err)
	}
}

// TestRunResolvesAdapters checks the adapters dir comes from -shell made
// absolute against the cwd, and from a .mro2nf.yml shell against the config
// file's dir (#266).
func TestRunResolvesAdapters(t *testing.T) {
	dir, err := filepath.Abs(filepath.Join("..", "..", "testdata", "file_min"))
	if err != nil {
		t.Fatal(err)
	}

	cwd, cfgDir := t.TempDir(), t.TempDir()
	cfg := filepath.Join(cfgDir, config.FileName)
	writeTestFile(t, cfg, "shell: tools/adapters/python/martian_shell.py\n")

	t.Chdir(cwd)

	for name, tc := range map[string]struct {
		flags []string
		want  string
	}{
		"flag":   {[]string{"-shell", "adapters/python/martian_shell.py"}, filepath.Join(cwd, "adapters")},
		"config": {[]string{"-config", cfg}, filepath.Join(cfgDir, "tools", "adapters")},
	} {
		t.Run(name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "proj")
			args := append([]string{"-o", out, "-mre", "/opt/bin/mre", "-mropath", dir}, tc.flags...)

			if err := run(append(args, filepath.Join(dir, "pipeline.mro"))); err != nil {
				t.Fatal(err)
			}

			if want := "params.mro2nf_adapters = '" + tc.want + "'"; !strings.Contains(readConfig(t, out), want) {
				t.Errorf("nextflow.config lacks %q:\n%s", want, readConfig(t, out))
			}
		})
	}
}

// TestApplyConfigPrecedence checks the .mro2nf.yml precedence: a config key sets
// a flag the user did not pass, but an explicit flag always wins.
func TestApplyConfigPrecedence(t *testing.T) {
	dir := t.TempDir()
	mro := filepath.Join(dir, "pipeline.mro")
	writeTestFile(t, filepath.Join(dir, config.FileName), "mre: /cfg/mre\nmonitor: true\n")

	for _, tc := range []struct {
		name        string
		args        []string
		wantMre     string
		wantMonitor bool
	}{
		{"unset_flags_take_config", []string{mro}, "/cfg/mre", true},
		// -monitor=false must beat the config's monitor: true.
		{"explicit_flags_win", []string{"-mre", "/flag/mre", "-monitor=false", mro}, "/flag/mre", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("t", flag.ContinueOnError)
			f := defineTranspileFlags(fs)

			if err := fs.Parse(tc.args); err != nil {
				t.Fatal(err)
			}

			if err := applyConfig(fs, "", mro, f.configPtrs()); err != nil {
				t.Fatal(err)
			}

			if *f.mre != tc.wantMre || *f.monitor != tc.wantMonitor {
				t.Errorf("mre=%q monitor=%v, want mre=%q monitor=%v", *f.mre, *f.monitor, tc.wantMre, tc.wantMonitor)
			}
		})
	}
}

// TestApplyConfigMonitorReachesOptions checks a monitor value sourced purely
// from the config reaches the emitted options.
func TestApplyConfigMonitorReachesOptions(t *testing.T) {
	dir := t.TempDir()
	mro := filepath.Join(dir, "pipeline.mro")
	writeTestFile(t, filepath.Join(dir, config.FileName), "monitor: true\nshell: "+testShell+"\n")

	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	f := defineTranspileFlags(fs)
	_ = fs.Parse([]string{mro})

	if err := applyConfig(fs, "", mro, f.configPtrs()); err != nil {
		t.Fatal(err)
	}

	opts, err := f.options(mro)
	if err != nil {
		t.Fatal(err)
	}

	if !opts.Monitor {
		t.Errorf("config-sourced monitor must reach options(): Monitor=%v", opts.Monitor)
	}
}

// TestToolPaths pins the tool-path rules: an exec'd tool's bare name (any
// name, not just "mre") stays a PATH command, while every other tool path —
// and a file argument even when bare — is made absolute, relative to base.
func TestToolPaths(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		resolve  func(p, base string) (string, error)
		in, base string
		want     string
	}{
		{"command/empty", commandPath, "", "", ""},
		{"command/bare_mre", commandPath, "mre", "", "mre"},
		{"command/any_bare_name", commandPath, "mrjob", "/cfg", "mrjob"},
		{"command/relative", commandPath, "bin/mre", "", filepath.Join(cwd, "bin", "mre")},
		{"command/dot_relative", commandPath, "./mre", "", filepath.Join(cwd, "mre")},
		{"command/relative_to_base", commandPath, "bin/mre", "/cfg", "/cfg/bin/mre"},
		{"command/relative_base", commandPath, "bin/mre", "cfg", filepath.Join(cwd, "cfg", "bin", "mre")},
		{"command/absolute", commandPath, "/usr/bin/mre", "/cfg", "/usr/bin/mre"},
		{"file/empty", filePath, "", "/cfg", ""},
		{"file/bare", filePath, "martian_shell.py", "", filepath.Join(cwd, "martian_shell.py")},
		{"file/bare_relative_to_base", filePath, "martian_shell.py", "/cfg", "/cfg/martian_shell.py"},
		{"file/relative", filePath, "py/martian_shell.py", "", filepath.Join(cwd, "py", "martian_shell.py")},
		{"file/absolute", filePath, "/opt/martian_shell.py", "/cfg", "/opt/martian_shell.py"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.resolve(tc.in, tc.base)
			if err != nil {
				t.Fatal(err)
			}

			if got != tc.want {
				t.Errorf("resolve(%q, %q) = %q, want %q", tc.in, tc.base, got, tc.want)
			}
		})
	}
}

// TestOptionsMrjob checks mrjob defaults to the one beside mre, a PATH
// command beside a bare mre, and that an explicit -mrjob wins.
func TestOptionsMrjob(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"beside_mre", []string{"-mre", "/opt/bin/mre"}, "/opt/bin/mrjob"},
		{"bare_mre", nil, "mrjob"},
		{"explicit", []string{"-mre", "/opt/bin/mre", "-mrjob", "/usr/bin/mrjob"}, "/usr/bin/mrjob"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("t", flag.ContinueOnError)
			f := defineTranspileFlags(fs)

			if err := fs.Parse(append(tc.args, "-shell", testShell, "p.mro")); err != nil {
				t.Fatal(err)
			}

			opts, err := f.options("p.mro")
			if err != nil {
				t.Fatal(err)
			}

			if opts.Mrjob != tc.want {
				t.Errorf("Mrjob = %q, want %q", opts.Mrjob, tc.want)
			}
		})
	}
}

// TestApplyConfigToolsRelativeToConfigFile checks a relative mre, shell or
// mrjob in a .mro2nf.yml resolves against the file's directory, not the
// working directory, under the CLI's rules (a bare mre or mrjob stays a PATH
// command, a bare shell is a file), and that an explicit flag is left for the
// working directory.
func TestApplyConfigToolsRelativeToConfigFile(t *testing.T) {
	t.Chdir(t.TempDir())

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	writeTestFile(t, filepath.Join("proj", config.FileName), "mre: mre\nshell: martian_shell.py\nmrjob: tools/mrjob\n")
	writeTestFile(t, filepath.Join("conf", "x.yml"), "mre: bin/mre\nshell: ../py/martian_shell.py\n")

	mro := filepath.Join("proj", "pipeline.mro")

	for _, tc := range []struct {
		name string
		args []string
		want [3]string
	}{
		{"alongside_mro", []string{mro}, [3]string{
			"mre", filepath.Join(cwd, "proj", "martian_shell.py"), filepath.Join(cwd, "proj", "tools", "mrjob"),
		}},
		{"explicit_config", []string{"-config", filepath.Join("conf", "x.yml"), mro}, [3]string{
			filepath.Join(cwd, "conf", "bin", "mre"), filepath.Join(cwd, "py", "martian_shell.py"), "",
		}},
		{"explicit_flag_wins", []string{"-shell", "s.py", mro}, [3]string{
			"mre", "s.py", filepath.Join(cwd, "proj", "tools", "mrjob"),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("t", flag.ContinueOnError)
			f := defineTranspileFlags(fs)

			if err := fs.Parse(tc.args); err != nil {
				t.Fatal(err)
			}

			if err := applyConfig(fs, *f.configPath, fs.Arg(0), f.configPtrs()); err != nil {
				t.Fatal(err)
			}

			if diff := cmp.Diff(tc.want, [3]string{*f.mre, *f.shell, *f.mrjob}); diff != "" {
				t.Errorf("mre, shell, mrjob (-want +got):\n%s", diff)
			}
		})
	}
}

// TestApplyConfigExplicitMissingIsError ensures an explicit -config path that
// does not exist fails loudly instead of silently dropping the user's defaults.
// The implicit alongside-the-.mro probe stays tolerant of a missing file.
func TestApplyConfigExplicitMissingIsError(t *testing.T) {
	dir := t.TempDir()
	mro := filepath.Join(dir, "pipeline.mro")

	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	f := defineTranspileFlags(fs)
	_ = fs.Parse([]string{mro})

	typo := filepath.Join(dir, "typo.yml")

	switch err := applyConfig(fs, typo, mro, f.configPtrs()); {
	case err == nil:
		t.Errorf("explicit -config %s is missing: want an error, got nil", typo)
	case !errors.Is(err, os.ErrNotExist):
		t.Errorf("want os.ErrNotExist for the missing config, got %v", err)
	case !strings.Contains(err.Error(), typo):
		t.Errorf("error should name the missing path %s, got %v", typo, err)
	}

	if err := applyConfig(fs, "", mro, f.configPtrs()); err != nil {
		t.Errorf("implicit probe with no config file: want no error, got %v", err)
	}
}

// TestBuildVersionReachesOptions checks the -ldflags version reaches the
// emitted options, whose every task names it in its -resume cache key
// (#449). A test binary has no stamped module version, so an unstamped build
// is dev.
func TestBuildVersionReachesOptions(t *testing.T) {
	for _, stamp := range []string{"v1.2.3-4-gabcdef0-dirty", "dev"} {
		t.Run(stamp, func(t *testing.T) {
			old := version
			version = stamp

			t.Cleanup(func() { version = old })

			fs := flag.NewFlagSet("t", flag.ContinueOnError)
			f := defineTranspileFlags(fs)
			_ = fs.Parse([]string{"-shell", testShell})

			opts, err := f.options("p.mro")
			if err != nil {
				t.Fatal(err)
			}

			if opts.Version != stamp {
				t.Errorf("Version = %q, want %q", opts.Version, stamp)
			}
		})
	}
}

// TestChooseVersion checks an unstamped build takes the module version Go
// stamps, so builds of different commits get different -resume cache keys
// (#449), and stays dev only when Go stamps none.
func TestChooseVersion(t *testing.T) {
	const pseudo = "v0.5.1-0.20261011171927-bab46362db39+dirty"

	withMain := func(v string) *debug.BuildInfo {
		return &debug.BuildInfo{Main: debug.Module{Path: "github.com/eunmann/mro2nf", Version: v}}
	}

	for _, tc := range []struct {
		name  string
		stamp string
		info  *debug.BuildInfo
		want  string
	}{
		{"ldflags_stamp_wins", "v1.2.3", withMain(pseudo), "v1.2.3"},
		{"module_version", "dev", withMain(pseudo), pseudo},
		{"devel", "dev", withMain("(devel)"), "dev"},
		{"empty_module_version", "dev", withMain(""), "dev"},
		{"no_build_info", "dev", nil, "dev"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := chooseVersion(tc.stamp, tc.info); got != tc.want {
				t.Errorf("chooseVersion(%q) = %q, want %q", tc.stamp, got, tc.want)
			}
		})
	}
}

// TestSearchPathReachesOptions checks the include paths the project compiles
// with: -mropath, else $MROPATH, else the .mro's directory, as mrp, each
// made absolute, each once, and never the .mro's directory the search path
// does not name.
func TestSearchPathReachesOptions(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	abs := func(dirs ...string) []string {
		out := make([]string, len(dirs))
		for i, d := range dirs {
			out[i] = filepath.Join(cwd, d)
		}

		return out
	}
	list := func(dirs ...string) string { return strings.Join(dirs, string(os.PathListSeparator)) }

	for _, tc := range []struct {
		name string
		env  string
		args []string
		want []string
	}{
		{"default", "", []string{"proj/p.mro"}, abs("proj")},
		{"env", list("m1", "m2", "m1"), []string{"proj/p.mro"}, abs("m1", "m2")},
		{"flag_beats_env", "m1", []string{"-mropath", "a", "proj/p.mro"}, abs("a")},
		{"explicit_cwd", "", []string{"-mropath", ".", "p.mro"}, []string{cwd}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MROPATH", tc.env)

			fs := flag.NewFlagSet("t", flag.ContinueOnError)
			f := defineTranspileFlags(fs)

			if err := fs.Parse(append([]string{"-shell", testShell}, tc.args...)); err != nil {
				t.Fatal(err)
			}

			opts, err := f.options(fs.Arg(0))
			if err != nil {
				t.Fatal(err)
			}

			if diff := cmp.Diff(tc.want, opts.MroPaths); diff != "" {
				t.Errorf("MroPaths (-want +got):\n%s", diff)
			}
		})
	}
}

const includingMRO = `@include "lib.mro"

pipeline EP(
    in  path reads,
    out int  total,
)
{
    call COUNT(
        reads = self.reads,
    )

    return (
        total = COUNT.total,
    )
}

call EP(
    reads = "input.txt",
)
`

const libMRO = `stage COUNT(
    in  path reads,
    out int  total,
    src py   "stages/count",
)
`

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestRunDefaultSearchPath checks @include is searched as mrp searches it
// when -mropath is not given: $MROPATH when set, else the .mro's directory —
// never the working directory, whose unrelated lib.mro would make the
// include ambiguous or resolve it to the wrong stages.
func TestRunDefaultSearchPath(t *testing.T) {
	cwd := t.TempDir()
	writeTestFile(t, filepath.Join(cwd, "lib.mro"), "stage OTHER(\n    src py \"stages/other\",\n)\n")
	t.Chdir(cwd)

	beside := t.TempDir()
	writeTestFile(t, filepath.Join(beside, "lib.mro"), libMRO)
	writeTestFile(t, filepath.Join(beside, "stages", "count", "__init__.py"), "")

	libDir := t.TempDir()
	writeTestFile(t, filepath.Join(libDir, "lib.mro"), libMRO)
	writeTestFile(t, filepath.Join(libDir, "stages", "count", "__init__.py"), "")

	for _, tc := range []struct{ name, env, dir string }{
		{"mro_dir", "", beside},
		{"env", libDir, t.TempDir()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MROPATH", tc.env)

			writeTestFile(t, filepath.Join(tc.dir, "input.txt"), "reads")
			mro := filepath.Join(tc.dir, "pipeline.mro")
			writeTestFile(t, mro, includingMRO)

			if err := run([]string{"-o", t.TempDir(), "-shell", testShell, mro}); err != nil {
				t.Errorf("transpile: %v", err)
			}
		})
	}
}

// TestRunRefusesPipelineMrpCannotInstantiate checks each pipeline mrp refuses
// before any stage runs (#387) fails the transpile, naming the call and
// binding as mrp does, and emits no project.
func TestRunRefusesPipelineMrpCannotInstantiate(t *testing.T) {
	refused := map[string]string{
		"rt_gate_split_reject":      "RTG.B input value:",
		"rt_gate_split_reject_sub":  "RTGS.B input value:",
		"rt_gate_split_reject_pass": "RTGP.B input value:",
	}

	for fixture, binding := range refused {
		t.Run(fixture, func(t *testing.T) {
			out, err := runFixture(t, fixture)
			if err == nil || !strings.Contains(err.Error(), binding) {
				t.Fatalf("run: %v, want mrp's refusal naming %q", err, binding)
			}

			if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a refused transpile left %s (stat: %v)", out, err)
			}
		})
	}
}

// TestRunRejectsDeletedFlags checks a deleted flag is an unknown flag, so a
// stale invocation fails instead of emitting a different project.
func TestRunRejectsDeletedFlags(t *testing.T) {
	mro := filepath.Join("..", "..", "testdata", "file_min", "pipeline.mro")

	for _, name := range []string{
		"emitter=v2", "native", "inline-pipelines=false", "target=awsbatch", "container=img",
		"allow-external-files", "fold-disables", "fuse-chains", "native-runner",
	} {
		t.Run(name, func(t *testing.T) {
			flagName, _, _ := strings.Cut(name, "=")

			err := run([]string{"-o", t.TempDir(), "-shell", testShell, "-" + name, mro})
			if err == nil || !strings.Contains(err.Error(), "flag provided but not defined: -"+flagName) {
				t.Errorf("run with -%s: want an undefined-flag error, got %v", name, err)
			}
		})
	}
}
