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
monitor: "false"   # inline comment kept out of the value
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Monitor == nil || *cfg.Monitor {
		t.Errorf("Monitor = %v, want false (quotes + inline comment stripped)", cfg.Monitor)
	}
}

func TestLoadMissingIsNotAnError(t *testing.T) {
	cfg, err := config.Load(filepath.Join(t.TempDir(), "nope.yml"))
	if err != nil {
		t.Fatalf("missing file: want no error, got %v", err)
	}

	if *cfg != (config.Config{}) {
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
		"unknown key":           "bogus: 1\n",
		"typo'd monitor key":    "monitors: true\n",
		"malformed line":        "no colon here\n",
		"non-bool":              "monitor: yes-please\n",
		"deleted native key":    "native: true\n",
		"deleted target key":    "target: awsbatch\n",
		"deleted container key": "container: img\n",
		"deleted fold key":      "fold-disables: true\n",
		"deleted mre key":       "mre: /opt/mre\n",
		"deleted shell key":     "shell: martian_shell.py\n",
		"deleted mrjob key":     "mrjob: /opt/mrjob\n",
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
