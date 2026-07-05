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
native: true
native-runner: false
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
	if cfg.Native == nil || !*cfg.Native {
		t.Errorf("Native = %v, want true", cfg.Native)
	}
	if cfg.NativeRunner == nil || *cfg.NativeRunner {
		t.Errorf("NativeRunner = %v, want false", cfg.NativeRunner)
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

func TestLoadRequiredMissingIsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope.yml")
	if _, err := config.LoadRequired(path); err == nil {
		t.Errorf("LoadRequired(%s): want an error for a missing file, got nil", path)
	}
}

func TestLoadRejectsBadInput(t *testing.T) {
	cases := map[string]string{
		"unknown key":       "bogus: 1\n",
		"typo'd native key": "natives: true\n",
		"malformed line":    "no colon here\n",
		"non-bool":          "fuse-chains: yes-please\n",
		"non-bool native":   "native: yes-please\n",
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := config.Load(write(t, body)); err == nil {
				t.Errorf("Load(%q): want an error, got nil", body)
			}
		})
	}
}

// TestLoadPropagatesNonNotExist pins the tolerance boundary: Load swallows only
// a genuinely-absent file. Any other open/read failure (here: the path is a
// directory) must surface, or the implicit probe would silently drop the user's
// defaults on an EISDIR/EACCES.
func TestLoadPropagatesNonNotExist(t *testing.T) {
	path := filepath.Join(t.TempDir(), config.FileName)
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := config.Load(path); err == nil {
		t.Errorf("Load(%s) where the path is a directory: want an error, got nil", path)
	}
}
