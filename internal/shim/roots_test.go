package shim_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/google/go-cmp/cmp"
)

// TestRootsHolds: roots hold each root and what lies under it once cleaned,
// a root under another adding nothing, so neither a sibling sharing a
// root's prefix nor a path climbing out of one reads as held; a relative
// path is no root, and the zero Roots hold nothing.
func TestRootsHolds(t *testing.T) {
	r := shim.NewRoots("/mnt/s3", "/data/ref/", "/data/ref/genome.fa", "rel/x", "")

	for p, want := range map[string]bool{
		"/mnt/s3":              true,
		"/mnt/s3/b/k.txt":      true,
		"/data/ref":            true,
		"/data/ref/genome.fa":  true,
		"/data/ref/./sub/x":    true,
		"/data/reference/x":    false,
		"/data/ref/../other/x": false,
		"/data":                false,
		"/mnt/s3x/b/k":         false,
		"rel/x":                false,
		"/rel/x":               false,
	} {
		if got := r.Holds(p); got != want {
			t.Errorf("Holds(%q) = %t, want %t", p, got, want)
		}
	}

	if d := cmp.Diff([]string{"/data/ref", "/mnt/s3"}, r.Paths()); d != "" {
		t.Errorf("Paths (-want +got):\n%s", d)
	}

	if (shim.Roots{}).Holds("/mnt/s3/b") || shim.NewRoots().Holds("/") {
		t.Error("no roots hold a path")
	}
}

// TestRootsExceptTheTaskDir: under a root that holds the task's own dir, as
// an entry input naming a directory above the run's scratch dir does, a
// stage's own output is still its own, copied into its outs bundle, while
// the input it was given under the root stays the path it is; Except
// recognizes the dir by its absolute and its real path.
func TestRootsExceptTheTaskDir(t *testing.T) {
	base := t.TempDir()
	input := writeSelfNamed(t, filepath.Join(base, "ref", "genome.fa"))
	own := writeSelfNamed(t, filepath.Join(base, "task", "main", "files", "out.txt"))

	link := filepath.Join(t.TempDir(), "task")
	if err := os.Symlink(filepath.Join(base, "task"), link); err != nil {
		t.Fatal(err)
	}

	roots, err := shim.NewRoots(base).Except(link)
	if err != nil {
		t.Fatal(err)
	}

	if !roots.Holds(input) || roots.Holds(own) || roots.Holds(filepath.Join(link, "main", "files", "out.txt")) {
		t.Fatalf("Holds: input %t, own output %t, want only the input held", roots.Holds(input), roots.Holds(own))
	}

	conf, err := shim.ConfineStage(filepath.Join(base, "task"), "main", true, roots)
	if err != nil {
		t.Fatal(err)
	}

	bundle := filepath.Join(t.TempDir(), "outs")
	payload := map[string]any{"out": own, "ref": input}

	if err := shim.WriteBundle(bundle, payload, fields(t, "out file", "ref file"), fileTable(t), shim.Opts{Confine: conf, Roots: roots}); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(bundle, "data.json"))
	if err != nil {
		t.Fatal(err)
	}

	var stored map[string]string
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}

	if stored["ref"] != input {
		t.Errorf("the input under the root is stored as %q, want its path %q", stored["ref"], input)
	}

	if leaf, _ := readPayload(t, bundle)["out"].(string); leaf == own || !filepath.IsLocal(mustRel(t, bundle, leaf)) {
		t.Errorf("the stage's own output reads back at %q, want a copy in the bundle", leaf)
	}
}

func mustRel(t *testing.T, base, p string) string {
	t.Helper()

	rel, err := filepath.Rel(base, p)
	if err != nil {
		t.Fatal(err)
	}

	return rel
}
