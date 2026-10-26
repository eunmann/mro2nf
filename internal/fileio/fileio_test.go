package fileio_test

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/eunmann/mro2nf/internal/fileio"
	"github.com/google/go-cmp/cmp"
)

// TestWriteJSONRoundTrip: WriteJSON creates the file's dir with mrp's
// modes, and ReadJSON reads back what it wrote.
func TestWriteJSONRoundTrip(t *testing.T) {
	old := syscall.Umask(0o022)
	t.Cleanup(func() { syscall.Umask(old) })

	path := filepath.Join(t.TempDir(), "a", "b", "rec.json")
	want := map[string]int{"index": 1, "count": 2}

	if err := fileio.WriteJSON(path, want); err != nil {
		t.Fatal(err)
	}

	for p, mode := range map[string]os.FileMode{path: fileio.FilePerm, filepath.Dir(path): fileio.DirPerm} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}

		if info.Mode().Perm() != mode {
			t.Errorf("%s mode %v, want %v", p, info.Mode().Perm(), mode)
		}
	}

	var got map[string]int
	if err := fileio.ReadJSON(path, &got); err != nil {
		t.Fatal(err)
	}

	if d := cmp.Diff(want, got); d != "" {
		t.Errorf("read back (-want +got):\n%s", d)
	}
}

// TestReadJSONFailsOnBadJSON: a record that is not JSON is an error.
func TestReadJSONFailsOnBadJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rec.json")
	if err := fileio.Write(path, []byte("{")); err != nil {
		t.Fatal(err)
	}

	var v any
	if err := fileio.ReadJSON(path, &v); err == nil {
		t.Error("ReadJSON of a truncated record succeeded, want an error")
	}
}
