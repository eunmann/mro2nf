//go:build e2e

package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestHashTree(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	files := map[string]string{
		"_outs":                "root metadata",
		"pipeline_outs.json":   "{}",
		"d/_outs":              "a stage output named like metadata",
		"d/.zattrs":            "dotfile",
		"d/sub/f.txt":          "f",
		"elsewhere/target.txt": "linked",
	}

	for rel, content := range files {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}

		writeTestFile(t, path, content)
	}

	for _, rel := range []string{"empty", "d/empty_sub"} {
		if err := os.MkdirAll(filepath.Join(dir, rel), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.Symlink(filepath.Join(dir, "elsewhere"), filepath.Join(dir, "linkdir")); err != nil {
		t.Fatal(err)
	}

	digest := func(s string) string {
		sum := sha256.Sum256([]byte(s))

		return hex.EncodeToString(sum[:])
	}

	want := map[string]string{
		"d/_outs":              digest("a stage output named like metadata"),
		"d/.zattrs":            digest("dotfile"),
		"d/sub/f.txt":          digest("f"),
		"d/empty_sub/":         emptyDirMark,
		"empty/":               emptyDirMark,
		"elsewhere/target.txt": digest("linked"),
		"linkdir/target.txt":   digest("linked"),
	}

	if diff := cmp.Diff(want, hashTree(t, dir, "_outs", "pipeline_outs.json")); diff != "" {
		t.Errorf("hashTree mismatch (-want +got):\n%s", diff)
	}
}

func TestHashTreeAbsentDir(t *testing.T) {
	t.Parallel()

	if got := hashTree(t, filepath.Join(t.TempDir(), "missing")); len(got) != 0 {
		t.Errorf("hashTree of an absent dir = %v, want empty", got)
	}
}

// TestPublishedTreeMarksLinks checks that, unless it follows links,
// publishedTree tells a file, a file in a directory and an empty directory
// reached through a symlink from a copy.
func TestPublishedTreeMarksLinks(t *testing.T) {
	t.Parallel()

	dir, elsewhere := t.TempDir(), t.TempDir()
	writeTestFile(t, filepath.Join(dir, "copy.txt"), "x")
	target := writeTestFile(t, filepath.Join(elsewhere, "target.txt"), "x")
	writeTestFile(t, filepath.Join(elsewhere, "tree", "in.txt"), "y")

	if err := os.MkdirAll(filepath.Join(elsewhere, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	links := map[string]string{
		"link.txt": target,
		"tree":     filepath.Join(elsewhere, "tree"),
		"empty":    filepath.Join(elsewhere, "empty"),
	}

	for link, to := range links {
		if err := os.Symlink(to, filepath.Join(dir, link)); err != nil {
			t.Fatal(err)
		}
	}

	digest := func(s string) string {
		sum := sha256.Sum256([]byte(s))

		return hex.EncodeToString(sum[:])
	}

	for _, follow := range []bool{true, false} {
		mark := linkMark
		if follow {
			mark = ""
		}

		want := map[string]string{
			"copy.txt":    digest("x"),
			"link.txt":    mark + digest("x"),
			"tree/in.txt": mark + digest("y"),
			"empty/":      mark + emptyDirMark,
		}

		got, err := publishedTree(dir, follow, nil)
		if err != nil {
			t.Fatal(err)
		}

		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("publishedTree(followLinks=%v) (-want +got):\n%s", follow, diff)
		}
	}
}
