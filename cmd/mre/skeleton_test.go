package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/eunmann/mro2nf/internal/shim"
)

// TestOutsExpandsStagedSkeletons (#610): a skeleton OUTS staged as one of
// its items, a link to the producer's .mre-skeleton.json, becomes the
// bundle dir it describes, under the name it was staged at; a staged dir
// is left as it is.
func TestOutsExpandsStagedSkeletons(t *testing.T) {
	t.Chdir(t.TempDir())

	writeOutsFile(t, filepath.Join("producer", "data.json"), `{"x":1}`)
	writeOutsFile(t, filepath.Join("producer", shim.SkeletonFile), `{"entries":[{"path":"data.json","record":"{\"x\":1}"},{"path":"f","dir":true},{"path":"f/L0000","dir":true},{"path":"f/L0000/big.txt"}]}`)
	writeOutsFile(t, filepath.Join("in", "u2", "data.json"), `{"y":2}`)

	abs, err := filepath.Abs(filepath.Join("producer", shim.SkeletonFile))
	if err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(abs, filepath.Join("in", "u1")); err != nil {
		t.Fatal(err)
	}

	if err := expandSkeletons([]string{"in", ""}); err != nil {
		t.Fatal(err)
	}

	for path, want := range map[string]string{"in/u1/data.json": `{"x":1}`, "in/u1/f/L0000/big.txt": "", "in/u2/data.json": `{"y":2}`} {
		if got, err := os.ReadFile(path); err != nil || string(got) != want {
			t.Errorf("%s = %q, %v, want %q", path, got, err, want)
		}
	}

	if _, err := os.Lstat(filepath.Join("in", "u1"+shim.SkeletonFile)); !os.IsNotExist(err) {
		t.Errorf("the staged skeleton is left beside its expansion: %v", err)
	}
}

// writeOutsFile writes body at path, creating its dir, and returns path.
func writeOutsFile(t *testing.T, path, body string) string {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	return path
}
