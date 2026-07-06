package main

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/config"
	"github.com/eunmann/mro2nf/internal/ir"
)

// TestApplyConfigPrecedence checks the .mro2nf.yml precedence: a config key sets
// a flag the user did not pass, but an explicit flag always wins.
func TestApplyConfigPrecedence(t *testing.T) {
	dir := t.TempDir()
	mro := filepath.Join(dir, "pipeline.mro")

	if err := os.WriteFile(filepath.Join(dir, config.FileName),
		[]byte("target: awsbatch\nfuse-chains: true\nnative: true\nnative-runner: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Unset flags take the config value.
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	target := fs.String("target", "local", "")
	fuse := fs.Bool("fuse-chains", false, "")
	native := fs.Bool("native", false, "")
	nativeRunner := fs.Bool("native-runner", false, "")
	_ = fs.Parse([]string{mro})

	ptrs := cliPtrs{target: target, fuseChains: fuse, native: native, nativeRunner: nativeRunner}
	if err := applyConfig(fs, "", mro, ptrs); err != nil {
		t.Fatal(err)
	}
	if *target != "awsbatch" || !*fuse {
		t.Errorf("config should set unset flags: target=%q fuse=%v", *target, *fuse)
	}
	if !*native || !*nativeRunner {
		t.Errorf("config should set unset native flags: native=%v native-runner=%v", *native, *nativeRunner)
	}

	// An explicitly-passed flag overrides the config — including a bool passed
	// as its default value (-native=false must beat the config's native: true).
	fs2 := flag.NewFlagSet("t", flag.ContinueOnError)
	target2 := fs2.String("target", "local", "")
	fuse2 := fs2.Bool("fuse-chains", false, "")
	native2 := fs2.Bool("native", false, "")
	nativeRunner2 := fs2.Bool("native-runner", false, "")
	_ = fs2.Parse([]string{"-target", "healthomics", "-native=false", "-native-runner=false", mro})

	ptrs2 := cliPtrs{target: target2, fuseChains: fuse2, native: native2, nativeRunner: nativeRunner2}
	if err := applyConfig(fs2, "", mro, ptrs2); err != nil {
		t.Fatal(err)
	}
	if *target2 != "healthomics" {
		t.Errorf("explicit -target should win over config, got %q", *target2)
	}
	if !*fuse2 {
		t.Errorf("unset fuse-chains should still take config value, got %v", *fuse2)
	}
	if *native2 || *nativeRunner2 {
		t.Errorf("explicit -native=false/-native-runner=false should win over config: native=%v native-runner=%v",
			*native2, *nativeRunner2)
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

	switch err := applyConfig(fs, typo, mro, cliPtrs{target: target}); {
	case err == nil:
		t.Errorf("explicit -config %s is missing: want an error, got nil", typo)
	case !errors.Is(err, os.ErrNotExist):
		t.Errorf("want os.ErrNotExist for the missing config, got %v", err)
	case !strings.Contains(err.Error(), typo):
		t.Errorf("error should name the missing path %s, got %v", typo, err)
	}

	// No explicit path: a missing probe file is still fine.
	if err := applyConfig(fs, "", mro, cliPtrs{target: target}); err != nil {
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
		[]byte("native: true\nnative-runner: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	f := defineTranspileFlags(fs)
	_ = fs.Parse([]string{mro})

	if err := applyConfig(fs, "", mro, f.configPtrs()); err != nil {
		t.Fatal(err)
	}

	opts, err := f.options()
	if err != nil {
		t.Fatal(err)
	}

	if !opts.Native || !opts.NativeRunner {
		t.Errorf("config-sourced native flags must reach options(): Native=%v NativeRunner=%v",
			opts.Native, opts.NativeRunner)
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
