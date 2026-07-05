package main

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/eunmann/mro2nf/internal/config"
)

// TestApplyConfigPrecedence checks the .mro2nf.yml precedence: a config key sets
// a flag the user did not pass, but an explicit flag always wins.
func TestApplyConfigPrecedence(t *testing.T) {
	dir := t.TempDir()
	mro := filepath.Join(dir, "pipeline.mro")

	if err := os.WriteFile(filepath.Join(dir, config.FileName),
		[]byte("target: awsbatch\nfuse-chains: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Unset flags take the config value.
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	target := fs.String("target", "local", "")
	fuse := fs.Bool("fuse-chains", false, "")
	_ = fs.Parse([]string{mro})

	if err := applyConfig(fs, "", mro, cliPtrs{target: target, fuseChains: fuse}); err != nil {
		t.Fatal(err)
	}
	if *target != "awsbatch" || !*fuse {
		t.Errorf("config should set unset flags: target=%q fuse=%v", *target, *fuse)
	}

	// An explicitly-passed flag overrides the config.
	fs2 := flag.NewFlagSet("t", flag.ContinueOnError)
	target2 := fs2.String("target", "local", "")
	fuse2 := fs2.Bool("fuse-chains", false, "")
	_ = fs2.Parse([]string{"-target", "healthomics", mro})

	if err := applyConfig(fs2, "", mro, cliPtrs{target: target2, fuseChains: fuse2}); err != nil {
		t.Fatal(err)
	}
	if *target2 != "healthomics" {
		t.Errorf("explicit -target should win over config, got %q", *target2)
	}
	if !*fuse2 {
		t.Errorf("unset fuse-chains should still take config value, got %v", *fuse2)
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
