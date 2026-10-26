package shim_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/eunmann/mro2nf/internal/types/typestest"
	"github.com/google/go-cmp/cmp"
)

// TestSkeletonIsTheBundleWithoutBytes (#610): a bundle's skeleton expands
// to the bundle's every path, its records as written, a record written
// into the bundle before WriteItem included, and each other file below its
// leaves empty, a dir leaf's empty dir and nested files among them. A
// dangling link is no path of the bundle but its leaf's shape record
// (#624).
func TestSkeletonIsTheBundleWithoutBytes(t *testing.T) {
	dir := t.TempDir()
	leaf := writeFileAt(t, filepath.Join(dir, "work", "a.txt"), "aaaa")
	tree := filepath.Join(dir, "work", "d")
	writeFileAt(t, filepath.Join(tree, "sub", "x.txt"), "x")

	if err := os.MkdirAll(filepath.Join(tree, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(filepath.Join(dir, "gone"), filepath.Join(tree, "broken")); err != nil {
		t.Fatal(err)
	}

	bundle := filepath.Join(dir, "bundle")
	writeFileAt(t, filepath.Join(bundle, "instance.json"), `{"node":"N"}`)
	writeBundle(t, bundle, map[string]any{"f": leaf, "d": tree}, typestest.Fields(t, "f file", "d file"), shim.Opts{})

	expanded := filepath.Join(dir, "expanded")
	if err := shim.ExpandSkeleton(filepath.Join(bundle, shim.SkeletonFile), expanded); err != nil {
		t.Fatal(err)
	}

	want := pathsOf(t, bundle)
	delete(want, shim.SkeletonFile)

	for p := range want {
		if strings.HasPrefix(p, "f/") && !strings.HasSuffix(p, "/") && filepath.Base(p) != ".mre-origin" && filepath.Base(p) != ".mre-shape" {
			want[p] = ""
		}
	}

	if d := cmp.Diff(want, pathsOf(t, expanded)); d != "" {
		t.Errorf("expanded skeleton (-bundle without bytes +got):\n%s", d)
	}

	for _, p := range []string{"instance.json", "f/L0001/d/sub/x.txt", "f/L0001/d/empty/", "f/L0001/.mre-shape"} {
		if _, ok := want[p]; !ok {
			t.Errorf("the bundle lacks %s: %v", p, want)
		}
	}

	if _, ok := want["f/L0001/d/broken"]; ok {
		t.Errorf("the bundle holds the dangling link: %v", want)
	}
}

// TestCopyShapeReadsNoBytes (#610): CopyShape places a tree's every dir
// and an empty file at each of its files, a file it reaches through a link
// included, as CopyTree copies the tree, and a dangling link as itself, as
// a reader restores it (#624).
func TestCopyShapeReadsNoBytes(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "d")
	writeFileAt(t, filepath.Join(src, "sub", "x.txt"), "x")
	other := writeFileAt(t, filepath.Join(dir, "other.txt"), "other")

	if err := os.Symlink(other, filepath.Join(src, "linked.txt")); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(filepath.Join(dir, "gone"), filepath.Join(src, "broken")); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(dir, "shape")
	if err := shim.CopyShape(src, dst); err != nil {
		t.Fatal(err)
	}

	want := map[string]string{"sub/": "", "sub/x.txt": "", "linked.txt": "", "broken": "-> " + filepath.Join(dir, "gone")}
	if d := cmp.Diff(want, pathsOf(t, dst)); d != "" {
		t.Errorf("shape (-want +got):\n%s", d)
	}
}

// TestExpandSkeletonStaysInItsDir: a skeleton entry naming a path outside
// the dir it expands into fails the expansion and writes nothing there.
func TestExpandSkeletonStaysInItsDir(t *testing.T) {
	dir := t.TempDir()
	skeleton := writeFileAt(t, filepath.Join(dir, "skeleton.json"), `{"entries":[{"path":"../escaped"}]}`)

	if err := shim.ExpandSkeleton(skeleton, filepath.Join(dir, "bundle")); err == nil {
		t.Error("a skeleton entry outside its dir expanded, want an error")
	}

	if _, err := os.Lstat(filepath.Join(dir, "escaped")); !os.IsNotExist(err) {
		t.Errorf("the escaping entry was written: %v", err)
	}
}

// pathsOf maps each path below dir, a dir by its path and a trailing "/",
// to a file's content, or a link's target after "-> ".
func pathsOf(t *testing.T, dir string) map[string]string {
	t.Helper()

	out := map[string]string{}

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == dir {
			return err
		}

		rel := filepath.ToSlash(strings.TrimPrefix(path, dir+string(filepath.Separator)))

		switch {
		case d.IsDir():
			out[rel+"/"] = ""

			return nil
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			out[rel] = "-> " + target

			return err
		}

		raw, err := os.ReadFile(path)
		out[rel] = string(raw)

		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	return out
}

// writeFileAt writes body at path, creating its dir, and returns path.
func writeFileAt(t *testing.T, path, body string) string {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	return path
}
