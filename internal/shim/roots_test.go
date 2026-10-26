package shim_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/eunmann/mro2nf/internal/types/typestest"
)

// holds is whether roots hold p, failing the test on an error.
func holds(t *testing.T, roots shim.Roots, p string) bool {
	t.Helper()

	held, err := roots.Holds(p)
	if err != nil {
		t.Fatalf("Holds(%q): %v", p, err)
	}

	return held
}

// TestRootsHolds: roots hold each root and what lies under it once cleaned,
// so neither a sibling sharing a root's prefix nor a path climbing out of
// one reads as held; a relative path is no root, and the zero Roots hold
// nothing.
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
		if got := holds(t, r, p); got != want {
			t.Errorf("Holds(%q) = %t, want %t", p, got, want)
		}
	}

	if holds(t, shim.Roots{}, "/mnt/s3/b") || holds(t, shim.NewRoots(), "/") {
		t.Error("no roots hold a path")
	}
}

// TestRootsExceptTheTaskDir: under a root that holds the task's own dir, as
// an entry input naming a directory above the run's scratch dir does, a
// stage's own output is still its own, copied into its outs bundle, while
// the input it was given under the root stays the path it is; Except
// recognizes the dir by its absolute and its real path.
func TestRootsExceptTheTaskDir(t *testing.T) {
	// macOS's temp dir is under /var, a link to /private/var: own must be
	// spelled by the task dir's absolute or real path, as Except names it.
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

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

	if !holds(t, roots, input) || holds(t, roots, own) || holds(t, roots, filepath.Join(link, "main", "files", "out.txt")) {
		t.Fatalf("Holds: input %t, own output %t, want only the input held", holds(t, roots, input), holds(t, roots, own))
	}

	conf, err := shim.ConfineStage(filepath.Join(base, "task"), "main", true, roots)
	if err != nil {
		t.Fatal(err)
	}

	bundle := filepath.Join(t.TempDir(), "outs")
	payload := map[string]any{"out": own, "ref": input}

	writeBundle(t, bundle, payload, typestest.Fields(t, "out file", "ref file"), shim.Opts{Confine: conf, Roots: roots})

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

// TestRootsHoldTheRealPath: a root given under a symlinked dir, as /home
// linking to /gpfs/home makes every host path, holds its real path too, so a
// stage output linking to the input it read in place resolves to an input,
// which strict confinement admits.
func TestRootsHoldTheRealPath(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	input := writeSelfNamed(t, filepath.Join(base, "real", "ref", "genome.fa"))

	home := filepath.Join(t.TempDir(), "home")
	if err := os.Symlink(filepath.Join(base, "real"), home); err != nil {
		t.Fatal(err)
	}

	roots := shim.NewRoots(filepath.Join(home, "ref"))

	if !holds(t, roots, input) || !holds(t, roots, filepath.Join(home, "ref", "genome.fa")) {
		t.Fatalf("Holds: real path %t, given path %t, want both held", holds(t, roots, input), holds(t, roots, filepath.Join(home, "ref", "genome.fa")))
	}

	task := filepath.Join(t.TempDir(), "task")

	out := filepath.Join(task, "main", "files", "ref")
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(input, out); err != nil {
		t.Fatal(err)
	}

	conf, err := shim.ConfineStage(task, "main", true, roots)
	if err != nil {
		t.Fatal(err)
	}

	bundle := filepath.Join(t.TempDir(), "outs")
	if _, err := shim.WriteItem(bundle, map[string]any{"ref": out}, typestest.Fields(t, "ref file"), fileTable(t), shim.Opts{Confine: conf, Roots: roots}, shim.Staged{}); err != nil {
		t.Fatalf("a stage output linking to the real path of an input read in place: %v", err)
	}
}

// TestTaskResolvesOnlyWhatItReads: a task of a run reading many entry files
// in place, each under a symlinked dir, that reads one of them, names it in
// its args, returns its path and links to it as given resolves no root's
// real path; a path only a real path holds resolves each root once for the
// task.
func TestTaskResolvesOnlyWhatItReads(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	home := filepath.Join(t.TempDir(), "home")
	if err := os.Symlink(base, home); err != nil {
		t.Fatal(err)
	}

	const n = 200

	values := make([]string, n)
	for i := range values {
		writeSelfNamed(t, filepath.Join(base, "in", fmt.Sprintf("e%03d.txt", i)))
		values[i] = filepath.Join(home, "in", fmt.Sprintf("e%03d.txt", i))
	}

	task := filepath.Join(t.TempDir(), "task")
	own := writeSelfNamed(t, filepath.Join(task, "main", "files", "own.txt"))

	link := filepath.Join(task, "main", "files", "ref")
	if err := os.Symlink(values[0], link); err != nil {
		t.Fatal(err)
	}

	given := shim.NewRoots(values...)
	resolved := shim.CountResolves(given)

	roots, err := given.Except(task)
	if err != nil {
		t.Fatal(err)
	}

	linkLeaves(t, filepath.Join(task, "args"), map[string]any{"in": values[0]}, typestest.Fields(t, "in file"), roots)

	conf, err := shim.ConfineStage(task, "main", true, roots)
	if err != nil {
		t.Fatal(err)
	}

	payload := map[string]any{"own": own, "ref": link, "same": values[0]}
	writeBundle(t, filepath.Join(t.TempDir(), "outs"), payload, typestest.Fields(t, "own file", "ref file", "same file"), shim.Opts{Confine: conf, Roots: roots})

	if *resolved != 0 {
		t.Errorf("the task resolved %d roots, want none", *resolved)
	}

	for range 2 {
		if !holds(t, roots, filepath.Join(base, "in", "e007.txt")) {
			t.Fatal("a root's real path is not held")
		}
	}

	if *resolved != n {
		t.Errorf("two paths only real paths hold resolved %d roots, want each of %d once", *resolved, n)
	}
}

// TestRootsUnresolvable: a path no root holds by its given path fails where
// a root's real path cannot be read for a reason other than its absence,
// rather than reading the root as its given path alone.
func TestRootsUnresolvable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a dir of mode 000")
	}

	locked := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(locked, 0o000); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	roots := shim.NewRoots(filepath.Join(locked, "ref"))

	if !holds(t, roots, filepath.Join(locked, "ref", "x")) {
		t.Error("the root's given path is not held")
	}

	if _, err := roots.Holds(filepath.Join(t.TempDir(), "x")); err == nil {
		t.Error("Holds of a path outside the given root, under a dir of mode 000: nil error, want the resolve failure")
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
