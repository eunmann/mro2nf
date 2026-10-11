package emit

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestCopyTreeMissingSource pins copyTree's hard-error contract directly:
// callers pre-verify sources (checkContainerSources), so a nonexistent src
// must return an error naming the path — never a silent no-op.
func TestCopyTreeMissingSource(t *testing.T) {
	src := filepath.Join(t.TempDir(), "does-not-exist")

	err := copyTree(src, filepath.Join(t.TempDir(), "dst"))
	if err == nil || !strings.Contains(err.Error(), src) {
		t.Errorf("copyTree(missing src): want error naming %q, got %v", src, err)
	}
}

func writeT(t *testing.T, path, content string, perm os.FileMode) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
}

func linkT(t *testing.T, target, link string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// treeEntries lists every path below dir, relative and slash-separated.
func treeEntries(t *testing.T, dir string) []string {
	t.Helper()

	var got []string

	err := filepath.WalkDir(dir, func(p string, _ os.DirEntry, err error) error {
		if err != nil || p == dir {
			return err
		}

		rel, err := filepath.Rel(dir, p)
		got = append(got, filepath.ToSlash(rel))

		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	sort.Strings(got)

	return got
}

// TestCopyTreeSkipsJunk checks a vendored stage dir leaves out VCS metadata,
// dotenv files, Python bytecode, and editor and OS litter at any depth, and
// keeps everything else.
func TestCopyTreeSkipsJunk(t *testing.T) {
	src := t.TempDir()
	for _, rel := range []string{
		".git/config", ".hg/hgrc", ".svn/entries", ".env", ".env.local", ".envrc",
		"__pycache__/mod.cpython-312.pyc", "old.pyc", "old.pyo", ".DS_Store", "Thumbs.db",
		".idea/workspace.xml", ".vscode/settings.json", "mod.py~", ".mod.py.swp", ".mod.py.swo",
		".#mod.py", "#mod.py#", "sub/.git", "sub/__pycache__/x.pyc", "sub/.env",
		"__init__.py", "sub/helper.py", "sub/data/table.tsv", ".config.yml",
	} {
		writeT(t, filepath.Join(src, rel), rel, 0o644)
	}

	dst := filepath.Join(t.TempDir(), "stage")
	if err := copyTree(src, dst); err != nil {
		t.Fatalf("copyTree: %v", err)
	}

	want := []string{".config.yml", "__init__.py", "sub", "sub/data", "sub/data/table.tsv", "sub/helper.py"}
	if diff := cmp.Diff(want, treeEntries(t, dst)); diff != "" {
		t.Errorf("vendored entries (-want +got):\n%s", diff)
	}
}

// TestCopyTreeKeepsInTreeLinks checks a symlink whose target is inside the
// vendored tree stays a symlink, rewritten relative so it resolves inside the
// image whatever its original spelling.
func TestCopyTreeKeepsInTreeLinks(t *testing.T) {
	src := t.TempDir()
	writeT(t, filepath.Join(src, "lib", "impl.py"), "impl", 0o644)
	linkT(t, "lib/impl.py", filepath.Join(src, "rel.py"))
	linkT(t, filepath.Join(src, "lib", "impl.py"), filepath.Join(src, "sub", "abs.py"))
	linkT(t, filepath.Join(src, "lib"), filepath.Join(src, "libdir"))

	dst := filepath.Join(t.TempDir(), "stage")
	if err := copyTree(src, dst); err != nil {
		t.Fatalf("copyTree: %v", err)
	}

	for rel, want := range map[string]string{
		"rel.py":     "lib/impl.py",
		"sub/abs.py": "../lib/impl.py",
		"libdir":     "lib",
	} {
		got, err := os.Readlink(filepath.Join(dst, rel))
		if err != nil {
			t.Errorf("%s: want a symlink: %v", rel, err)

			continue
		}

		if got != want {
			t.Errorf("%s -> %q, want %q", rel, got, want)
		}
	}

	if got := readT(t, filepath.Join(dst, "sub", "abs.py")); got != "impl" {
		t.Errorf("sub/abs.py resolves to %q, want the vendored impl", got)
	}
}

func readT(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	return string(data)
}

// TestCopyTreeRefusesLeavingLinks checks a symlink whose target is not
// vendored fails the copy instead of shipping a host file (or a link that
// would dangle in the image), and that nothing it points at was copied.
func TestCopyTreeRefusesLeavingLinks(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, src, outside string)
	}{
		{"file_outside", func(t *testing.T, src, outside string) {
			t.Helper()
			linkT(t, filepath.Join(outside, "secret"), filepath.Join(src, "creds"))
		}},
		{"dir_outside", func(t *testing.T, src, outside string) {
			t.Helper()
			linkT(t, outside, filepath.Join(src, "sub", "home"))
		}},
		{"relative_escape", func(t *testing.T, src, outside string) {
			t.Helper()
			rel, err := filepath.Rel(filepath.Join(src, "sub"), filepath.Join(outside, "secret"))
			if err != nil {
				t.Fatal(err)
			}
			linkT(t, rel, filepath.Join(src, "sub", "creds"))
		}},
		{"dangling", func(t *testing.T, src, _ string) {
			t.Helper()
			linkT(t, "gone", filepath.Join(src, "creds"))
		}},
		{"into_junk", func(t *testing.T, src, _ string) {
			t.Helper()
			writeT(t, filepath.Join(src, ".env"), "SECRET", 0o600)
			linkT(t, ".env", filepath.Join(src, "creds"))
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src, outside := t.TempDir(), t.TempDir()
			writeT(t, filepath.Join(src, "__init__.py"), "", 0o644)
			writeT(t, filepath.Join(outside, "secret"), "SECRET", 0o600)
			tc.setup(t, src, outside)

			dst := filepath.Join(t.TempDir(), "stage")
			if err := copyTree(src, dst); err == nil {
				t.Fatal("copyTree: want an error for a link leaving the tree")
			}

			_ = filepath.WalkDir(dst, func(p string, d os.DirEntry, err error) error {
				if err == nil && d.Type().IsRegular() && readT(t, p) == "SECRET" {
					t.Errorf("secret copied to %s", p)
				}

				return nil
			})
		})
	}
}

// TestCopyTreeMode checks a vendored file keeps its own permission bits and a
// linked src its target's, never the link's 0777, and that a re-vendor over
// a wider stale copy does not keep the stale mode.
func TestCopyTreeMode(t *testing.T) {
	src := t.TempDir()
	writeT(t, filepath.Join(src, "dir", "secret.cfg"), "s", 0o600)
	writeT(t, filepath.Join(src, "dir", "run.sh"), "r", 0o750)
	linkT(t, filepath.Join(src, "dir", "secret.cfg"), filepath.Join(src, "link.cfg"))

	dst := t.TempDir()
	writeT(t, filepath.Join(dst, "dir", "secret.cfg"), "stale", 0o777)

	if err := copyTree(filepath.Join(src, "dir"), filepath.Join(dst, "dir")); err != nil {
		t.Fatalf("copyTree dir: %v", err)
	}

	if err := copyTree(filepath.Join(src, "link.cfg"), filepath.Join(dst, "top.cfg")); err != nil {
		t.Fatalf("copyTree linked file: %v", err)
	}

	for rel, want := range map[string]os.FileMode{
		"dir/secret.cfg": 0o600,
		"dir/run.sh":     0o750,
		"top.cfg":        0o600,
	} {
		info, err := os.Lstat(filepath.Join(dst, rel))
		if err != nil {
			t.Fatal(err)
		}

		if got := info.Mode().Perm(); got&^want != 0 || got&0o700 != want&0o700 {
			t.Errorf("%s mode = %o, want %o (never wider)", rel, got, want)
		}
	}
}
