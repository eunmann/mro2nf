package vendormartian_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	vendormartian "github.com/eunmann/mro2nf/vendor-martian"
	"github.com/google/go-cmp/cmp"
)

// TestAdaptersEmbedEveryVendoredFile checks Adapters holds each file of
// python/, byte for byte, and nothing else: go:embed leaves out a file
// whose name starts with '.' or '_', so a vendored __init__.py would
// otherwise drop out of every run unseen. A __pycache__ a local Python run
// leaves in python/ is Python's, not an adapter.
func TestAdaptersEmbedEveryVendoredFile(t *testing.T) {
	want := map[string]string{}

	err := filepath.WalkDir("python", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() && d.Name() == "__pycache__" {
			return filepath.SkipDir
		}

		if d.IsDir() {
			return nil
		}

		raw, err := os.ReadFile(path)
		want[filepath.ToSlash(path)] = string(raw)

		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]string{}

	err = fs.WalkDir(vendormartian.Adapters, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}

		raw, err := vendormartian.Adapters.ReadFile(path)
		got[path] = string(raw)

		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("embedded adapters differ from python/ (-want +got):\n%s", diff)
	}

	if _, ok := got["python/martian_shell.py"]; !ok {
		t.Error("Adapters lacks python/martian_shell.py, which mrjob runs a py stage with")
	}
}
