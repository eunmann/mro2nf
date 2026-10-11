package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/apperror"
	"github.com/eunmann/mro2nf/internal/config"
	"github.com/eunmann/mro2nf/internal/emit"
	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/google/go-cmp/cmp"
)

// TestApplyConfigPrecedence checks the .mro2nf.yml precedence: a config key sets
// a flag the user did not pass, but an explicit flag always wins.
func TestApplyConfigPrecedence(t *testing.T) {
	dir := t.TempDir()
	mro := filepath.Join(dir, "pipeline.mro")

	if err := os.WriteFile(filepath.Join(dir, config.FileName),
		[]byte("target: awsbatch\nmonitor: true\nnative: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Unset flags take the config value.
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	target := fs.String("target", "local", "")
	monitor := fs.Bool("monitor", false, "")
	native := fs.Bool("native", false, "")
	_ = fs.Parse([]string{mro})

	ptrs := cliPtrs{target: target, monitor: monitor, native: native}
	if _, err := applyConfig(fs, "", mro, ptrs); err != nil {
		t.Fatal(err)
	}
	if *target != "awsbatch" || !*monitor || !*native {
		t.Errorf("config should set unset flags: target=%q monitor=%v native=%v", *target, *monitor, *native)
	}

	// An explicitly-passed flag overrides the config — including a bool passed
	// as its default value (-native=false must beat the config's native: true).
	fs2 := flag.NewFlagSet("t", flag.ContinueOnError)
	target2 := fs2.String("target", "local", "")
	monitor2 := fs2.Bool("monitor", false, "")
	native2 := fs2.Bool("native", false, "")
	_ = fs2.Parse([]string{"-target", "healthomics", "-native=false", mro})

	ptrs2 := cliPtrs{target: target2, monitor: monitor2, native: native2}
	if _, err := applyConfig(fs2, "", mro, ptrs2); err != nil {
		t.Fatal(err)
	}
	if *target2 != "healthomics" {
		t.Errorf("explicit -target should win over config, got %q", *target2)
	}
	if !*monitor2 {
		t.Errorf("unset monitor should still take config value, got %v", *monitor2)
	}
	if *native2 {
		t.Errorf("explicit -native=false should win over config, got native=%v", *native2)
	}
}

// TestAbsOrSelf pins the tool-path resolution rule: a bare command name (no
// path separator) is left for PATH lookup — ANY name, not just "mre" — while
// anything containing a separator is anchored to an absolute path so the
// generated project does not depend on Nextflow's task working directory.
func TestAbsOrSelf(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, in, want string
	}{
		{"empty stays empty", "", ""},
		{"bare mre stays PATH-resolved", "mre", "mre"},
		{"any bare command stays PATH-resolved", "mrjob", "mrjob"},
		{"relative path absolutized", "bin/mre", filepath.Join(cwd, "bin", "mre")},
		{"dot-relative path absolutized", "./mre", filepath.Join(cwd, "mre")},
		{"absolute path unchanged", "/usr/bin/mre", "/usr/bin/mre"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := absOrSelf(tc.in); got != tc.want {
				t.Errorf("absOrSelf(%q) = %q, want %q", tc.in, got, tc.want)
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
	target := fs.String("target", "local", "")
	_ = fs.Parse([]string{mro})

	typo := filepath.Join(dir, "typo.yml")

	switch _, err := applyConfig(fs, typo, mro, cliPtrs{target: target}); {
	case err == nil:
		t.Errorf("explicit -config %s is missing: want an error, got nil", typo)
	case !errors.Is(err, os.ErrNotExist):
		t.Errorf("want os.ErrNotExist for the missing config, got %v", err)
	case !strings.Contains(err.Error(), typo):
		t.Errorf("error should name the missing path %s, got %v", typo, err)
	}

	// No explicit path: a missing probe file is still fine.
	if _, err := applyConfig(fs, "", mro, cliPtrs{target: target}); err != nil {
		t.Errorf("implicit probe with no config file: want no error, got %v", err)
	}
}

// TestApplyConfigNativeReachesOptions pins the structural parity: a native
// value sourced purely from the config (never passed as a flag) must reach
// the single options() value both Diagnose and Emit consume — a reintroduced
// pre-config snapshot would silently split them.
func TestApplyConfigNativeReachesOptions(t *testing.T) {
	dir := t.TempDir()
	mro := filepath.Join(dir, "pipeline.mro")

	if err := os.WriteFile(filepath.Join(dir, config.FileName),
		[]byte("native: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	f := defineTranspileFlags(fs)
	_ = fs.Parse([]string{mro})

	applied, err := applyConfig(fs, "", mro, f.configPtrs())
	if err != nil {
		t.Fatal(err)
	}

	opts, err := f.options(fs, applied)
	if err != nil {
		t.Fatal(err)
	}

	if !opts.Native {
		t.Errorf("config-sourced native must reach options(): Native=%v", opts.Native)
	}
}

// TestBuildVersionReachesOptions checks the -ldflags version reaches Emit,
// whose every process script names it as the task's -resume cache key (#449).
// A test binary has no stamped module version, so an unstamped build is dev.
func TestBuildVersionReachesOptions(t *testing.T) {
	for _, stamp := range []string{"v1.2.3-4-gabcdef0-dirty", "dev"} {
		t.Run(stamp, func(t *testing.T) {
			old := version
			version = stamp

			t.Cleanup(func() { version = old })

			fs := flag.NewFlagSet("t", flag.ContinueOnError)
			f := defineTranspileFlags(fs)

			opts, err := f.options(fs, nil)
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

// TestSourceRootFlagsReachOptions checks an explicit -mropath reaches Emit
// split into the roots that bound copied files while the implicit "." adds
// none, and that copying an external file takes the explicit
// -allow-external-files.
func TestSourceRootFlagsReachOptions(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		wantPath  []string
		wantAllow bool
	}{
		{"default", []string{"p.mro"}, nil, false},
		{"explicit_cwd", []string{"-mropath", ".", "p.mro"}, []string{"."}, false},
		{"opt_in", []string{"-mropath", "a" + string(os.PathListSeparator) + "b", "-allow-external-files", "p.mro"}, []string{"a", "b"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("t", flag.ContinueOnError)
			f := defineTranspileFlags(fs)

			if err := fs.Parse(tc.args); err != nil {
				t.Fatal(err)
			}

			opts, err := f.options(fs, nil)
			if err != nil {
				t.Fatal(err)
			}

			if diff := cmp.Diff(tc.wantPath, opts.MROPath); diff != "" {
				t.Errorf("MROPath (-want +got):\n%s", diff)
			}

			if opts.AllowExternalFiles != tc.wantAllow {
				t.Errorf("AllowExternalFiles = %v, want %v", opts.AllowExternalFiles, tc.wantAllow)
			}
		})
	}
}

// TestStageCodePathsAbsolutizesNoRejoin guards the #178 review: a relative,
// already-declaring-dir-resolved SrcPath must be absolutized against the cwd, not
// re-joined against the entry .mro's dir (which would double-prefix an @included
// stage's path). The helper takes no entry dir, so a revert to a mroDir-join
// would not even compile against this test.
func TestStageCodePathsAbsolutizesNoRejoin(t *testing.T) {
	prog := &ir.Program{
		Stages: map[string]*ir.Stage{
			"S": {Name: "S", SrcPath: "proj/sub/code.py"}, // relative, declaring-dir-resolved
			"A": {Name: "A", SrcPath: "/abs/code.py"},     // absolute passes through
		},
	}

	code, err := stageCodePaths(prog)
	if err != nil {
		t.Fatalf("stageCodePaths: %v", err)
	}

	cwd, _ := os.Getwd()
	if want := filepath.Join(cwd, "proj", "sub", "code.py"); code["S"] != want {
		t.Errorf("S = %q, want %q (cwd-absolutized, not entry-dir-joined)", code["S"], want)
	}
	if code["A"] != "/abs/code.py" {
		t.Errorf("A = %q, want the absolute path unchanged", code["A"])
	}
}

// TestStageCodePathsKeepsBareCommand guards that a comp/exec stage whose src is a
// bare command name (no path separator, resolved on PATH at exec time — e.g.
// CellRanger's `cr_lib`) passes through verbatim rather than being cwd-absolutized
// into a broken filesystem path. Py stages and any path with a separator are still
// absolutized.
func TestStageCodePathsKeepsBareCommand(t *testing.T) {
	prog := &ir.Program{
		Stages: map[string]*ir.Stage{
			"C": {Name: "C", Lang: ir.LangComp, SrcPath: "cr_lib"}, // bare command → PATH
			"E": {Name: "E", Lang: ir.LangExec, SrcPath: "mytool"}, // bare command → PATH
			"P": {Name: "P", Lang: ir.LangPy, SrcPath: "code.py"},  // py leaf → absolutized
		},
	}

	code, err := stageCodePaths(prog)
	if err != nil {
		t.Fatalf("stageCodePaths: %v", err)
	}

	if code["C"] != "cr_lib" {
		t.Errorf("C = %q, want %q (bare command kept verbatim for PATH resolution)", code["C"], "cr_lib")
	}
	if code["E"] != "mytool" {
		t.Errorf("E = %q, want %q (bare command kept verbatim for PATH resolution)", code["E"], "mytool")
	}
	cwd, _ := os.Getwd()
	if want := filepath.Join(cwd, "code.py"); code["P"] != want {
		t.Errorf("P = %q, want %q (py leaf still cwd-absolutized)", code["P"], want)
	}
}

const sharedMRO = `stage COUNT(
    in  path reads,
    out int  total,
    src py   "stages/count",
)

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
    reads = %q,
)
`

// writeSharedPipeline writes a pipeline .mro with its stage code into dir, as
// a shared repository ships it, with def as the reads default.
func writeSharedPipeline(t *testing.T, dir, def string) string {
	t.Helper()

	writeTestFile(t, filepath.Join(dir, "stages", "count", "__init__.py"), "")
	writeTestFile(t, filepath.Join(dir, "input.txt"), "reads")

	mro := filepath.Join(dir, "pipeline.mro")
	writeTestFile(t, mro, fmt.Sprintf(sharedMRO, def))

	return mro
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestRunDefaultMROPathIsNotARoot checks the implicit -mropath of the working
// directory does not admit an entry default under it: transpiling a shared
// .mro from $HOME must not stage a file elsewhere in $HOME. An explicit
// -mropath still does.
func TestRunDefaultMROPathIsNotARoot(t *testing.T) {
	home := t.TempDir()
	secret := filepath.Join(home, "secret.txt")
	writeTestFile(t, secret, "SECRET")

	mro := writeSharedPipeline(t, filepath.Join(home, "shared"), secret)
	t.Chdir(home)

	if err := run([]string{"-o", t.TempDir(), mro}); !errors.Is(err, emit.ErrExternalFile) {
		t.Errorf("run from the default's parent: want ErrExternalFile, got %v", err)
	}

	if err := run([]string{"-o", t.TempDir(), "-mropath", ".", mro}); err != nil {
		t.Errorf("run with an explicit -mropath holding the default: %v", err)
	}
}

// TestRunRefusesPipelineMrpCannotInstantiate checks each pipeline mrp refuses
// before any stage runs (#387) fails the transpile in every emission mode,
// naming the call and binding as mrp does, and emits no project.
func TestRunRefusesPipelineMrpCannotInstantiate(t *testing.T) {
	refused := map[string]string{
		"rt_gate_split_reject":      "RTG.B input value:",
		"rt_gate_split_reject_sub":  "RTGS.B input value:",
		"rt_gate_split_reject_pass": "RTGP.B input value:",
	}

	modes := map[string][]string{
		"default":   nil,
		"native":    {"-native"},
		"no-inline": {"-inline-pipelines=false"},
	}

	for fixture, binding := range refused {
		for mode, flags := range modes {
			t.Run(fixture+"/"+mode, func(t *testing.T) {
				out := filepath.Join(t.TempDir(), "proj")
				mro := filepath.Join("..", "..", "testdata", fixture, "pipeline.mro")

				err := run(append(append([]string{"-o", out}, flags...), mro))
				if !errors.Is(err, apperror.ErrParse) {
					t.Fatalf("run: want ErrParse, got %v", err)
				}

				for _, want := range []string{binding, "unexpected array"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not contain %q", err, want)
					}
				}

				if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("a refused transpile left %s (stat: %v)", out, err)
				}
			})
		}
	}
}

// TestRunRejectsDeletedFlags checks a deleted emission-mode flag is an unknown
// flag, so a stale invocation fails instead of emitting a different project.
func TestRunRejectsDeletedFlags(t *testing.T) {
	mro := filepath.Join("..", "..", "testdata", "file_min", "pipeline.mro")

	for _, name := range []string{"fold-disables", "fuse-chains", "native-runner"} {
		t.Run(name, func(t *testing.T) {
			err := run([]string{"-o", t.TempDir(), "-" + name, mro})
			if err == nil || !strings.Contains(err.Error(), "flag provided but not defined: -"+name) {
				t.Errorf("run with -%s: want an undefined-flag error, got %v", name, err)
			}
		})
	}
}

// TestRunRefusesConfigToolOutsideRoots checks a .mro2nf.yml shipped beside a
// shared .mro cannot have a container target bake a host file as a tool,
// while the same path passed as a flag is the user's choice and is baked.
func TestRunRefusesConfigToolOutsideRoots(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "credentials")
	writeTestFile(t, secret, "SECRET")

	mre := filepath.Join(t.TempDir(), "mre")
	writeTestFile(t, mre, "#!/bin/sh\n")

	shared := t.TempDir()
	mro := writeSharedPipeline(t, shared, "input.txt")
	writeTestFile(t, filepath.Join(shared, config.FileName), "mrjob: "+secret+"\n")

	base := []string{"-target", "awsbatch", "-container", "img", "-mre", mre}

	err := run(append(append([]string{"-o", t.TempDir()}, base...), mro))
	if !errors.Is(err, emit.ErrExternalFile) {
		t.Errorf("config-set -mrjob outside the roots: want ErrExternalFile, got %v", err)
	}

	if err := run(append(append([]string{"-o", t.TempDir(), "-mrjob", secret}, base...), mro)); err != nil {
		t.Errorf("explicit -mrjob: %v", err)
	}
}
