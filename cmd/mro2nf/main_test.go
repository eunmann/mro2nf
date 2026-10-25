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

	"github.com/google/go-cmp/cmp"
)

// runFixture runs mro2nf on a testdata fixture into a fresh dir and returns
// the project dir and the error.
func runFixture(t *testing.T, fixture string, flags ...string) (string, error) {
	t.Helper()

	out := filepath.Join(t.TempDir(), "proj")
	dir := filepath.Join("..", "..", "testdata", fixture)
	args := append([]string{"-o", out, "-mropath", dir}, flags...)

	return out, run(append(args, filepath.Join(dir, "pipeline.mro")))
}

func readConfig(t *testing.T, proj string) string {
	t.Helper()

	cfg, err := os.ReadFile(filepath.Join(proj, "conf", "base.config"))
	if err != nil {
		t.Fatal(err)
	}

	return string(cfg)
}

// TestRunEmitsCallGraphProject checks a transpile of a py pipeline that
// names no tools emits the call-graph project, whose tasks run the mre on
// PATH with its own mrjob and adapters, unconfined unless the run says so.
func TestRunEmitsCallGraphProject(t *testing.T) {
	out, err := runFixture(t, "file_min")
	if err != nil {
		t.Fatal(err)
	}

	cfg := readConfig(t, out)
	for _, want := range []string{"params.mro2nf_mre = 'mre'", "params.mro2nf_mrjob = null", "params.mro2nf_adapters = null", "params.mro2nf_confine = false", "params.mro2nf_s3_mount = null", "params.x = null"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("conf/base.config lacks %q:\n%s", want, cfg)
		}
	}

	if _, err := os.Stat(filepath.Join(out, "stage.nf")); err != nil {
		t.Errorf("no stage module: %v", err)
	}
}

// TestUsageShowsDefaults checks the transpile takes only -o, -mropath and
// -version, and the usage text lists each with its default.
func TestUsageShowsDefaults(t *testing.T) {
	fs := flag.NewFlagSet("mro2nf", flag.ContinueOnError)

	var usage bytes.Buffer

	fs.SetOutput(&usage)
	defineTranspileFlags(fs)
	fs.PrintDefaults()

	var names []string

	fs.VisitAll(func(f *flag.Flag) { names = append(names, f.Name) })

	if diff := cmp.Diff([]string{"mropath", "o", "version"}, names); diff != "" {
		t.Errorf("flags (-want +got):\n%s", diff)
	}

	for _, want := range []string{"-mropath", `(default "out")`, "-version"} {
		if !strings.Contains(usage.String(), want) {
			t.Errorf("usage lacks %s:\n%s", want, usage.String())
		}
	}
}

// TestRunRefusesBadInvocations checks a transpile fails, emitting no
// project, on an undefined flag, a missing .mro or one too many and a .mro
// that does not exist, and that -version transpiles nothing.
func TestRunRefusesBadInvocations(t *testing.T) {
	mro := filepath.Join("..", "..", "testdata", "file_min", "pipeline.mro")
	out := filepath.Join(t.TempDir(), "proj")

	for name, args := range map[string][]string{
		"flag":    {"-bogus", mro},
		"no mro":  {"-o", out},
		"two mro": {"-o", out, mro, mro},
		"mro":     {"-o", out, filepath.Join(t.TempDir(), "absent.mro")},
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

// TestRunReadsNoConfigFile checks a .mro2nf.yml beside the .mro sets
// nothing: the project is the command line's alone.
func TestRunReadsNoConfigFile(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, ".mro2nf.yml"), "monitor: true\n")
	writeTestFile(t, filepath.Join(dir, "lib.mro"), libMRO)
	writeTestFile(t, filepath.Join(dir, "stages", "count", "__init__.py"), "")
	writeTestFile(t, filepath.Join(dir, "input.txt"), "reads")

	mro := filepath.Join(dir, "pipeline.mro")
	writeTestFile(t, mro, includingMRO)

	out := t.TempDir()
	if err := run([]string{"-o", out, mro}); err != nil {
		t.Fatal(err)
	}

	if cfg := readConfig(t, out); !strings.Contains(cfg, "params.mro2nf_monitor = false") {
		t.Errorf("a .mro2nf.yml beside the .mro set the project's monitor:\n%s", cfg)
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
			_ = fs.Parse(nil)

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

			if err := fs.Parse(tc.args); err != nil {
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

			if err := run([]string{"-o", t.TempDir(), mro}); err != nil {
				t.Errorf("transpile: %v", err)
			}
		})
	}
}

// TestRunRefusesPipelineMrpCannotInstantiate checks a pipeline mrp refuses
// before any stage runs, here a disable gate bound to null, fails the
// transpile with mrp's binding error, and emits no project.
func TestRunRefusesPipelineMrpCannotInstantiate(t *testing.T) {
	dir := t.TempDir()
	mro := filepath.Join(dir, "pipeline.mro")
	src := `stage WORK(in int x, out int y, src py "stages/work",)

pipeline P(in int x, in bool b, out int y,)
{
    call WORK(x = self.x,) using (disabled = self.b,)
    return (y = WORK.y,)
}

call P(x = 3, b = null,)
`

	if err := os.WriteFile(mro, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "proj")

	err := run([]string{"-o", out, "-mropath", dir, mro})
	if err == nil || !strings.Contains(err.Error(), "disabled cannot be bound to a null value") {
		t.Fatalf("run: %v, want mrp's refusal of the null gate", err)
	}

	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused transpile left %s (stat: %v)", out, err)
	}
}

// TestRunRejectsDeletedFlags checks a deleted flag is an unknown flag, so a
// stale invocation fails instead of emitting a different project.
func TestRunRejectsDeletedFlags(t *testing.T) {
	mro := filepath.Join("..", "..", "testdata", "file_min", "pipeline.mro")

	for _, name := range []string{
		"emitter=v2", "native", "inline-pipelines=false", "target=awsbatch", "container=img",
		"allow-external-files", "fold-disables", "fuse-chains", "native-runner",
		"mre=/opt/mre", "shell=martian_shell.py", "mrjob=/opt/mrjob", "config=.mro2nf.yml", "monitor", "confine-outputs",
	} {
		t.Run(name, func(t *testing.T) {
			flagName, _, _ := strings.Cut(name, "=")

			err := run([]string{"-o", t.TempDir(), "-" + name, mro})
			if err == nil || !strings.Contains(err.Error(), "flag provided but not defined: -"+flagName) {
				t.Errorf("run with -%s: want an undefined-flag error, got %v", name, err)
			}
		})
	}
}
