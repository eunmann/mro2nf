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
	if err := applyConfig(fs, typo, mro, cliPtrs{target: target}); err == nil {
		t.Errorf("explicit -config %s is missing: want an error, got nil", typo)
	}

	// No explicit path: a missing probe file is still fine.
	if err := applyConfig(fs, "", mro, cliPtrs{target: target}); err != nil {
		t.Errorf("implicit probe with no config file: want no error, got %v", err)
	}
}
