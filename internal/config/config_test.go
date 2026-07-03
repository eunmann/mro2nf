package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/eunmann/mro2nf/internal/config"
)

func write(t *testing.T, body string) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, config.FileName)

	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	return path
}

func TestLoad(t *testing.T) {
	cfg, err := config.Load(write(t, `
# a project's transpiler defaults
target: awsbatch
container: "ecr/repo:1"   # inline comment kept out of the value
fuse-chains: true
fold-disables: false
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Target == nil || *cfg.Target != "awsbatch" {
		t.Errorf("Target = %v, want awsbatch", cfg.Target)
	}
	if cfg.Container == nil || *cfg.Container != "ecr/repo:1" {
		t.Errorf("Container = %v, want ecr/repo:1 (quotes + inline comment stripped)", cfg.Container)
	}
	if cfg.FuseChains == nil || !*cfg.FuseChains {
		t.Errorf("FuseChains = %v, want true", cfg.FuseChains)
	}
	if cfg.FoldDisables == nil || *cfg.FoldDisables {
		t.Errorf("FoldDisables = %v, want false", cfg.FoldDisables)
	}
	// An unset key stays nil so the CLI keeps the flag default.
	if cfg.Monitor != nil {
		t.Errorf("Monitor = %v, want nil (unset)", cfg.Monitor)
	}
}

func TestLoadMissingIsNotAnError(t *testing.T) {
	cfg, err := config.Load(filepath.Join(t.TempDir(), "nope.yml"))
	if err != nil {
		t.Fatalf("missing file: want no error, got %v", err)
	}

	if cfg.Target != nil || cfg.FuseChains != nil {
		t.Errorf("missing file: want zero Config, got %+v", cfg)
	}
}

func TestLoadRejectsBadInput(t *testing.T) {
	cases := map[string]string{
		"unknown key":    "bogus: 1\n",
		"malformed line": "no colon here\n",
		"non-bool":       "fuse-chains: yes-please\n",
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := config.Load(write(t, body)); err == nil {
				t.Errorf("Load(%q): want an error, got nil", body)
			}
		})
	}
}
