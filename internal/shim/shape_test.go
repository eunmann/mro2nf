package shim_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/google/go-cmp/cmp"
)

// TestReaderRestoresDanglingLinks (#624): a link a stage wrote into a dir
// output that reaches nothing reaches the dir's reader with its target text,
// as mrp hands the reader the dir its producer wrote: the bundle records it
// beside the leaf rather than hold it, which an upload would refuse, and a
// reader restores it in the copy of the bundle an object store kept, and in
// the bundle itself, as a task on a shared work dir stages it, where a
// second reader finds it restored.
func TestReaderRestoresDanglingLinks(t *testing.T) {
	dir := t.TempDir()
	tree := filepath.Join(dir, "work", "d")
	writeFileAt(t, filepath.Join(tree, "real.txt"), "real")
	symlinkAt(t, "real.txt", filepath.Join(tree, "rel_link.txt"))
	symlinkAt(t, "/nonexistent", filepath.Join(tree, "dangling"))

	bundle := filepath.Join(dir, "bundle")
	writeBundle(t, bundle, map[string]any{"d": tree}, fields(t, "d path"), shim.Opts{})

	if links := linksBelow(t, bundle); len(links) != 0 {
		t.Errorf("the bundle holds links %v, which its upload would refuse", links)
	}

	for name, staged := range map[string]string{"object store copy": objectStoreCopy(t, bundle, filepath.Join(dir, "copy")), "shared work dir": bundle} {
		for range 2 {
			if err := mustStaged(t, staged).Restore(); err != nil {
				t.Fatalf("%s: restore: %v", name, err)
			}
		}

		d, _ := readPayload(t, staged)["d"].(string)
		if got := listDir(t, d); !slices.Equal(got, []string{"dangling", "real.txt", "rel_link.txt"}) {
			t.Errorf("%s: the reader lists %v, want the three entries mrp's reader lists", name, got)
		}

		if target, err := os.Readlink(filepath.Join(d, "dangling")); err != nil || target != "/nonexistent" {
			t.Errorf("%s: dangling = %q, %v, want the link to /nonexistent the stage wrote", name, target, err)
		}
	}
}

// TestReaderRestoresEmptyDirs (#624): an empty dir a stage wrote into a dir
// output, nested or the output itself, reaches the dir's reader from the
// copy of the bundle an object store kept, which holds no empty dir.
func TestReaderRestoresEmptyDirs(t *testing.T) {
	dir := t.TempDir()
	tree := filepath.Join(dir, "work", "d")
	writeFileAt(t, filepath.Join(tree, "sub", "x.txt"), "x")
	empty := filepath.Join(dir, "work", "e")

	for _, d := range []string{filepath.Join(tree, "empty"), filepath.Join(tree, "deep", "er"), empty} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	bundle := filepath.Join(dir, "bundle")
	writeBundle(t, bundle, map[string]any{"d": tree, "e": empty}, fields(t, "d path", "e path"), shim.Opts{})

	staged := objectStoreCopy(t, bundle, filepath.Join(dir, "copy"))
	if err := mustStaged(t, staged).Restore(); err != nil {
		t.Fatalf("restore: %v", err)
	}

	payload := readPayload(t, staged)
	d, _ := payload["d"].(string)
	e, _ := payload["e"].(string)

	want := map[string]string{"sub/": "", "sub/x.txt": "x", "empty/": "", "deep/": "", "deep/er/": ""}
	if diff := cmp.Diff(want, pathsOf(t, d)); diff != "" {
		t.Errorf("the reader's d (-written +read):\n%s", diff)
	}

	if names := listDir(t, e); len(names) != 0 {
		t.Errorf("the reader's e holds %v, want the empty dir the stage wrote", names)
	}
}

// TestRestoreRefusesADifferentEntry: a staged leaf holding, where its shape
// records a link, anything but that link fails the reader rather than hand
// the stage a dir its producer did not write.
func TestRestoreRefusesADifferentEntry(t *testing.T) {
	dir := t.TempDir()
	tree := filepath.Join(dir, "work", "d")
	symlinkAt(t, "/nonexistent", filepath.Join(tree, "dangling"))

	bundle := filepath.Join(dir, "bundle")
	writeBundle(t, bundle, map[string]any{"d": tree}, fields(t, "d path"), shim.Opts{})
	writeFileAt(t, filepath.Join(bundle, "f", "L0000", "d", "dangling"), "not a link")

	if err := mustStaged(t, bundle).Restore(); err == nil {
		t.Error("restored a leaf holding a file where its shape records a link, want an error")
	}
}

// TestShapeIsAReservedLeafName: a leaf named like the shape record it would
// share its ordinal dir with fails the write rather than overwrite either.
func TestShapeIsAReservedLeafName(t *testing.T) {
	dir := t.TempDir()
	src := writeFileAt(t, filepath.Join(dir, ".mre-shape"), "x")

	_, err := shim.WriteItem(filepath.Join(dir, "bundle"), map[string]any{"f": src}, fields(t, "f file"), fileTable(t), shim.Opts{}, shim.Staged{})
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Errorf("WriteItem of a leaf named .mre-shape = %v, want a reserved-name error", err)
	}
}

// objectStoreCopy copies the tree at src to dst as an upload to an object
// store and a download from it leave it, and returns dst: each regular file,
// below the dirs that hold one, and no link or empty dir.
func objectStoreCopy(t *testing.T, src, dst string) string {
	t.Helper()

	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}

		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		writeFileAt(t, filepath.Join(dst, strings.TrimPrefix(path, src)), string(raw))

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	return dst
}

// mustStaged is the task's staged items at paths.
func mustStaged(t *testing.T, paths ...string) shim.Staged {
	t.Helper()

	s, err := shim.NewStaged(paths...)
	if err != nil {
		t.Fatal(err)
	}

	return s
}

// listDir is the sorted names of the entries of dir.
func listDir(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}

	return names
}

// linksBelow is the paths of the links below dir.
func linksBelow(t *testing.T, dir string) []string {
	t.Helper()

	var links []string

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.Type()&fs.ModeSymlink != 0 {
			links = append(links, path)
		}

		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	return links
}

// symlinkAt links link to target, creating link's dir.
func symlinkAt(t *testing.T, target, link string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}
