package shim_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/eunmann/mro2nf/internal/types/typestest"
	"github.com/google/go-cmp/cmp"
)

// mountedTree lays out a root holding bucket b with a file and a
// directory, as the S3 mount shows a bucket, and returns the roots it is and
// the two leaf paths, the directory's spelled with a trailing slash.
func mountedTree(t *testing.T) (shim.Roots, string, string) {
	t.Helper()

	root := filepath.Join(t.TempDir(), "s3")
	file := writeSelfNamed(t, filepath.Join(root, "b", "reads", "r1.fastq"))
	writeSelfNamed(t, filepath.Join(root, "b", "ref", "genome.fa"))

	roots, err := shim.NewRoots(root)
	if err != nil {
		t.Fatal(err)
	}

	return roots, file, filepath.Join(root, "b", "ref") + "/"
}

// TestMountedLeafStaysItsPath: a file leaf under a root is the same path in
// every task, as the path mrp passes a stage, so every bundle writer and
// the args' linking keep it as the string it is, under a strict confinement
// too, with nothing of its tree copied or linked, and it reads back
// unchanged.
func TestMountedLeafStaysItsPath(t *testing.T) {
	roots, file, dir := mountedTree(t)
	params := typestest.Fields(t, "f file", "d path")
	payload := map[string]any{"f": file, "d": dir}

	work := t.TempDir()
	if err := os.MkdirAll(filepath.Join(work, "main"), 0o755); err != nil {
		t.Fatal(err)
	}

	conf, err := shim.ConfineStage(work, "main", true, roots)
	if err != nil {
		t.Fatal(err)
	}

	writers := map[string]func(t *testing.T, dir string) map[string]any{
		"WriteItem": func(t *testing.T, b string) map[string]any {
			t.Helper()

			writeBundle(t, b, payload, params, shim.Opts{Confine: conf, Roots: roots})

			wantStored(t, b, payload)

			return readPayload(t, b)
		},
		"LinkLeaves": func(t *testing.T, b string) map[string]any {
			t.Helper()

			return linkLeaves(t, b, payload, params, roots)
		},
	}

	for name, write := range writers {
		t.Run(name, func(t *testing.T) {
			bundle := filepath.Join(t.TempDir(), "bundle")
			got := write(t, bundle)

			if _, err := os.Lstat(filepath.Join(bundle, "f")); !os.IsNotExist(err) {
				t.Errorf("the bundle has a files dir (%v), want none", err)
			}

			if d := cmp.Diff(payload, got); d != "" {
				t.Errorf("payload read back (-want +got):\n%s", d)
			}
		})
	}
}

// wantStored requires the bundle at dir to store payload as it is.
func wantStored(t *testing.T, dir string, payload map[string]any) {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(dir, "data.json"))
	if err != nil {
		t.Fatal(err)
	}

	var stored map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}

	if d := cmp.Diff(payload, stored); d != "" {
		t.Errorf("data.json (-want +got):\n%s", d)
	}
}

// TestMountHolds: the mount holds its root and what lies under it once
// cleaned, so neither a sibling sharing its prefix nor a path climbing out
// of it reads as mounted, and no mount holds anything.
func TestMountHolds(t *testing.T) {
	m := shim.Mount("/mnt/s3")

	for p, want := range map[string]bool{
		"/mnt/s3":          true,
		"/mnt/s3/b/k.txt":  true,
		"/mnt/s3/b/ref/":   true,
		"/mnt/s3/b/./k":    true,
		"/mnt/s3x/b/k":     false,
		"/mnt/s3/../etc":   false,
		"/mnt/s3/b/../../": false,
		"mnt/s3/b/k":       false,
	} {
		if got := m.Holds(p); got != want {
			t.Errorf("Holds(%q) = %t, want %t", p, got, want)
		}
	}

	if shim.Mount("").Holds("/mnt/s3/b") {
		t.Error("the zero mount holds a path")
	}

	if _, err := shim.ParseMount("mnt/s3"); err == nil {
		t.Error("ParseMount accepted a relative root")
	}
}

// TestConfineAdmitsLinkIntoMount: a stage output that is the stage's own
// symlink under a root reaches what the stage was given, so a strict
// confinement stages it, as a staged input's, where with no roots it refuses
// a target outside the task.
func TestConfineAdmitsLinkIntoMount(t *testing.T) {
	roots, file, _ := mountedTree(t)
	params := typestest.Fields(t, "out file")

	work := t.TempDir()
	link := filepath.Join(work, "main", "files", "r1.fastq")

	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}

	for _, rs := range []shim.Roots{roots, {}} {
		conf, err := shim.ConfineStage(work, "main", true, rs)
		if err != nil {
			t.Fatal(err)
		}

		bundle := filepath.Join(t.TempDir(), "outs")
		_, err = shim.WriteItem(bundle, map[string]any{"out": link}, params, fileTable(t), shim.Opts{Confine: conf, Roots: rs}, shim.Staged{})

		if !rs.Holds(file) {
			if err == nil {
				t.Error("a strict confinement with no roots staged a link outside the task")
			}

			continue
		}

		if err != nil {
			t.Fatalf("strict confinement under the roots: %v", err)
		}

		leaf, _ := readPayload(t, bundle)["out"].(string)
		if got, err := os.ReadFile(leaf); err != nil || string(got) != file {
			t.Errorf("staged output reads %q (%v), want the mounted file's bytes", got, err)
		}
	}
}
