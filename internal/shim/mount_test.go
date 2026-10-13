package shim_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/google/go-cmp/cmp"
)

// mountedTree lays out a mount root holding bucket b with a file and a
// directory, and returns the mount and the two leaf paths, the directory's
// spelled with a trailing slash.
func mountedTree(t *testing.T) (shim.Mount, string, string) {
	t.Helper()

	root := filepath.Join(t.TempDir(), "s3")
	file := writeSelfNamed(t, filepath.Join(root, "b", "reads", "r1.fastq"))
	writeSelfNamed(t, filepath.Join(root, "b", "ref", "genome.fa"))

	return shim.Mount(root), file, filepath.Join(root, "b", "ref") + "/"
}

// TestMountedLeafStaysItsPath: a file leaf under the mount is the same path
// in every task, as the path mrp passes a stage, so every bundle writer keeps
// it as the string it is, under a strict confinement too, with nothing of
// its tree copied or linked into the bundle, and it reads back unchanged.
func TestMountedLeafStaysItsPath(t *testing.T) {
	mount, file, dir := mountedTree(t)
	params := fields(t, "f file", "d path")
	payload := map[string]any{"f": file, "d": dir}

	work := t.TempDir()
	if err := os.MkdirAll(filepath.Join(work, "main"), 0o755); err != nil {
		t.Fatal(err)
	}

	conf, err := shim.ConfineStage(work, "main", true, mount)
	if err != nil {
		t.Fatal(err)
	}

	writers := map[string]func(dir string) error{
		"WriteBundle": func(b string) error {
			return shim.WriteBundle(b, payload, params, fileTable(t), shim.Opts{Confine: conf, Mount: mount})
		},
		"WriteLinkedBundle": func(b string) error {
			return shim.WriteLinkedBundle(b, payload, params, fileTable(t), mount)
		},
	}

	for name, write := range writers {
		t.Run(name, func(t *testing.T) {
			bundle := filepath.Join(t.TempDir(), "bundle")
			if err := write(bundle); err != nil {
				t.Fatal(err)
			}

			if _, err := os.Lstat(filepath.Join(bundle, "f")); !os.IsNotExist(err) {
				t.Errorf("the bundle has a files dir (%v), want none", err)
			}

			if d := cmp.Diff(payload, readPayload(t, bundle)); d != "" {
				t.Errorf("payload read back (-want +got):\n%s", d)
			}

			raw, err := os.ReadFile(filepath.Join(bundle, "data.json"))
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
		})
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
// symlink into the mount reaches what the stage was given, so a strict
// confinement stages it, as a staged input's, where without the mount it
// refuses a target outside the task.
func TestConfineAdmitsLinkIntoMount(t *testing.T) {
	mount, file, _ := mountedTree(t)
	params := fields(t, "out file")

	work := t.TempDir()
	link := filepath.Join(work, "main", "files", "r1.fastq")

	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}

	for _, m := range []shim.Mount{mount, ""} {
		conf, err := shim.ConfineStage(work, "main", true, m)
		if err != nil {
			t.Fatal(err)
		}

		bundle := filepath.Join(t.TempDir(), "outs")
		err = shim.WriteBundle(bundle, map[string]any{"out": link}, params, fileTable(t), shim.Opts{Confine: conf, Mount: m})

		if m == "" {
			if err == nil {
				t.Error("a strict confinement with no mount staged a link outside the task")
			}

			continue
		}

		if err != nil {
			t.Fatalf("strict confinement under the mount: %v", err)
		}

		leaf, _ := readPayload(t, bundle)["out"].(string)
		if got, err := os.ReadFile(leaf); err != nil || string(got) != file {
			t.Errorf("staged output reads %q (%v), want the mounted file's bytes", got, err)
		}
	}
}
