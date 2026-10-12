//go:build e2e

package e2e

import (
	"bytes"
	"debug/elf"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

// TestBinariesStaticAndPathFree checks the harness builds mro2nf, mre and
// mrjob the way a release does: a cgo binary needs the builder's glibc or newer (a
// 2.34-linked mro2nf fails on Amazon Linux 2), and a path left in the binary
// leaks the builder's home directory into every image that bakes mre.
func TestBinariesStaticAndPathFree(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("static-linkage check reads ELF binaries")
	}

	buildBinaries(t)

	for _, bin := range slices.Sorted(maps.Keys(builtBinaries)) {
		path := filepath.Join(root, bin)

		f, err := elf.Open(path)
		if err != nil {
			t.Fatalf("open %s: %v", bin, err)
		}

		libs, err := f.ImportedLibraries()
		_ = f.Close()

		if err != nil {
			t.Fatalf("%s: read dynamic libraries: %v", bin, err)
		}

		if len(libs) != 0 {
			t.Errorf("%s links %v; build with CGO_ENABLED=0", bin, libs)
		}

		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", bin, err)
		}

		if bytes.Contains(raw, []byte(root)) {
			t.Errorf("%s contains the build directory %s; build with -trimpath", bin, root)
		}
	}
}
