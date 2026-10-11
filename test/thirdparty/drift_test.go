package thirdparty

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

const vendorMartian = "vendor-martian"

// martianSources maps each top-level entry of vendor-martian/ to the path in
// the Martian module it copies.
func martianSources() map[string]string {
	return map[string]string{licenseName: licenseName, "python": "adapters/python"}
}

// driftedFiles returns each file under vendorDir that is not byte-identical
// to its source in moduleDir, where sources maps a top-level entry of
// vendorDir to its path in the module, with the reason.
func driftedFiles(vendorDir, moduleDir string, sources map[string]string) ([]string, error) {
	var drift []string

	err := filepath.WalkDir(vendorDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Python writes __pycache__ beside the adapters whenever a test runs
		// them; it is gitignored, not vendored.
		if d.IsDir() && d.Name() == "__pycache__" {
			return filepath.SkipDir
		}

		if d.IsDir() {
			return nil
		}

		rel, err := filepath.Rel(vendorDir, path)
		if err != nil {
			return fmt.Errorf("rel %s: %w", path, err)
		}

		top, rest, _ := strings.Cut(filepath.ToSlash(rel), "/")

		src, ok := sources[top]
		if !ok {
			drift = append(drift, rel+": no source in the module is declared for "+top)

			return nil
		}

		upstream := filepath.Join(moduleDir, filepath.FromSlash(src), filepath.FromSlash(rest))

		want, err := os.ReadFile(upstream)
		if err != nil {
			drift = append(drift, fmt.Sprintf("%s: %v", rel, err))

			return nil
		}

		got, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}

		if !bytes.Equal(got, want) {
			drift = append(drift, rel+": differs from "+upstream)
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", vendorDir, err)
	}

	return drift, nil
}

// TestVendoredMartianMatchesModule fails, naming each file, when
// vendor-martian/ drifts from the Martian module go.mod pins, so a go.mod bump
// cannot leave the runtime adapters on another Martian than the parser.
func TestVendoredMartianMatchesModule(t *testing.T) {
	dir, err := ModuleDir(t.Context(), repoRoot, Vendored()[vendorMartian])
	if err != nil {
		t.Fatal(err)
	}

	drift, err := driftedFiles(filepath.Join(repoRoot, vendorMartian), dir, martianSources())
	if err != nil {
		t.Fatal(err)
	}

	if len(drift) > 0 {
		t.Errorf("%s/ drifted from %s; copy the pinned files in:\n%s", vendorMartian, dir, strings.Join(drift, "\n"))
	}
}

func TestDriftedFiles(t *testing.T) {
	vendor, module := t.TempDir(), t.TempDir()

	for path, content := range map[string]string{
		filepath.Join(vendor, "LICENSE"):                          "license",
		filepath.Join(vendor, "python", "same.py"):                "same",
		filepath.Join(vendor, "python", "changed.py"):             "vendored",
		filepath.Join(vendor, "python", "gone.py"):                "gone",
		filepath.Join(vendor, "extra", "x.txt"):                   "x",
		filepath.Join(vendor, "python", "__pycache__", "m.pyc"):   "bytecode",
		filepath.Join(module, "LICENSE"):                          "license",
		filepath.Join(module, "adapters", "python", "same.py"):    "same",
		filepath.Join(module, "adapters", "python", "changed.py"): "upstream",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	drift, err := driftedFiles(vendor, module, martianSources())
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		"extra/x.txt: no source in the module is declared for extra",
		"python/changed.py: differs from " + filepath.Join(module, "adapters", "python", "changed.py"),
		"python/gone.py: open " + filepath.Join(module, "adapters", "python", "gone.py") + ": no such file or directory",
	}
	if diff := cmp.Diff(want, drift); diff != "" {
		t.Errorf("driftedFiles (-want +got):\n%s", diff)
	}
}
