package shim

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// treeContents collects a directory tree as relative-path -> file content, so
// two trees can be compared structurally.
func treeContents(t *testing.T, root string) map[string]string {
	t.Helper()

	got := map[string]string{}

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		got[rel] = string(data)

		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	return got
}

// TestCopyTreeDirectoryRecursion checks CopyTree recurses into a directory
// source (a Martian directory output): a nested subdir and its files arrive at
// the destination with identical structure and content.
func TestCopyTreeDirectoryRecursion(t *testing.T) {
	dir := t.TempDir()

	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "b.txt"), []byte("beta"), 0o644); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(dir, "dst")
	if err := CopyTree(src, dst); err != nil {
		t.Fatalf("CopyTree: %v", err)
	}

	want := map[string]string{
		"a.txt":                       "alpha",
		filepath.Join("sub", "b.txt"): "beta",
	}
	if diff := cmp.Diff(want, treeContents(t, dst)); diff != "" {
		t.Errorf("copied tree mismatch (-want +got):\n%s", diff)
	}
}

// TestCopyTreeRefusesExistingDest guards the hard-link corruption fence: when
// linking fails (here deterministically, because the destination already
// exists, so link(2) returns EEXIST) CopyTree must refuse to fall back to a
// truncating byte copy over the existing file — that file may be a hard link
// shared with another bundle, and truncation would corrupt the shared inode.
func TestCopyTreeRefusesExistingDest(t *testing.T) {
	dir := t.TempDir()

	src := filepath.Join(dir, "src.txt")
	if err := os.WriteFile(src, []byte("new content"), 0o644); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(dir, "dst.txt")
	if err := os.WriteFile(dst, []byte("linked elsewhere"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := CopyTree(src, dst)
	if !errors.Is(err, errDestExists) {
		t.Fatalf("CopyTree over an existing dst = %v, want errDestExists", err)
	}

	// The existing destination must be untouched (no truncating copy happened).
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "linked elsewhere" {
		t.Errorf("existing dst content = %q, want it untouched", data)
	}
}

// TestCopyFileContents checks the byte copy CopyTree falls back to where a
// hard link is unavailable, as across devices from a cloud scratch volume: it
// keeps the content and the source's permission bits, and an unreadable
// source or an uncreatable destination fails naming the path.
func TestCopyFileContents(t *testing.T) {
	dir := t.TempDir()

	src := filepath.Join(dir, "src.sh")
	if err := os.WriteFile(src, []byte("#!/bin/sh\n"), 0o750); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(dir, "dst.sh")
	if err := copyFileContents(src, dst, info); err != nil {
		t.Fatalf("copyFileContents: %v", err)
	}

	got, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}

	if data, _ := os.ReadFile(dst); string(data) != "#!/bin/sh\n" || got.Mode().Perm() != info.Mode().Perm() || os.SameFile(info, got) {
		t.Errorf("copy = %q mode %v (same inode %v), want a separate %q with the source's mode %v",
			data, got.Mode().Perm(), os.SameFile(info, got), "#!/bin/sh\n", info.Mode().Perm())
	}

	cases := []struct {
		name, src, dst, msg string
	}{
		{"source_absent", filepath.Join(dir, "absent"), filepath.Join(dir, "out1"), "open "},
		{"dest_dir_absent", src, filepath.Join(dir, "no", "out2"), "create "},
		{"source_is_dir", dir, filepath.Join(dir, "out3"), "copy "},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := copyFileContents(tc.src, tc.dst, info)
			if err == nil || !strings.HasPrefix(err.Error(), tc.msg) {
				t.Errorf("copyFileContents(%s, %s) = %v, want an error starting %q", tc.src, tc.dst, err, tc.msg)
			}
		})
	}
}
