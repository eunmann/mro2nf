package emit

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestWriteFileSkipsUnchanged guards the #188 -resume fix: a byte-identical
// rewrite must preserve the file's mtime (skip the write) so a re-transpile does
// not bust Nextflow's default -resume cache for the staged _assets, while a real
// content change is still written.
func TestWriteFileSkipsUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "asset.json")

	if err := writeFile(path, []byte("hello")); err != nil {
		t.Fatalf("initial write: %v", err)
	}

	old := time.Unix(1_000_000, 0)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}

	// Identical rewrite: the write is skipped, so mtime is preserved.
	if err := writeFile(path, []byte("hello")); err != nil {
		t.Fatalf("identical rewrite: %v", err)
	}
	if fi, _ := os.Stat(path); !fi.ModTime().Equal(old) {
		t.Errorf("identical rewrite must preserve mtime (skip write); mtime = %v, want %v", fi.ModTime(), old)
	}

	// Changed content: the file is rewritten with the new bytes.
	if err := writeFile(path, []byte("world")); err != nil {
		t.Fatalf("changed rewrite: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "world" {
		t.Errorf("changed content must be written; got %q", got)
	}
}
