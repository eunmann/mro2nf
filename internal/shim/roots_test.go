package shim_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/eunmann/mro2nf/internal/types/typestest"
)

// TestRootsHolds: roots hold each root and what lies under it once cleaned,
// a root under another adding nothing, so neither a sibling sharing a
// root's prefix nor a path climbing out of one reads as held; a relative
// path is no root, and the zero Roots hold nothing.
func TestRootsHolds(t *testing.T) {
	r, err := shim.NewRoots("/mnt/s3", "/data/ref/", "/data/ref/genome.fa", "rel/x", "")
	if err != nil {
		t.Fatal(err)
	}

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

	none, err := shim.NewRoots()
	if err != nil {
		t.Fatal(err)
	}

	if (shim.Roots{}).Holds("/mnt/s3/b") || none.Holds("/") {
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

	roots, err := shim.NewRoots(base)
	if err != nil {
		t.Fatal(err)
	}

	if roots, err = roots.Except(link); err != nil {
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

	roots, err := shim.NewRoots(filepath.Join(home, "ref"))
	if err != nil {
		t.Fatal(err)
	}

	if !roots.Holds(input) || !roots.Holds(filepath.Join(home, "ref", "genome.fa")) {
		t.Fatalf("Holds: real path %t, given path %t, want both held", roots.Holds(input), roots.Holds(filepath.Join(home, "ref", "genome.fa")))
	}

	task := filepath.Join(t.TempDir(), "task")

	out := filepath.Join(task, "main", "files", "ref")
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(filepath.Join(home, "ref"), out); err != nil {
		t.Fatal(err)
	}

	conf, err := shim.ConfineStage(task, "main", true, roots)
	if err != nil {
		t.Fatal(err)
	}

	bundle := filepath.Join(t.TempDir(), "outs")
	if _, err := shim.WriteItem(bundle, map[string]any{"ref": out}, typestest.Fields(t, "ref path"), fileTable(t), shim.Opts{Confine: conf, Roots: roots}, shim.Staged{}); err != nil {
		t.Fatalf("a stage output linking to an input read in place: %v", err)
	}
}

// TestCodeRootsKeepPathsNotLinks (#625): under strict confinement a stage
// output naming a file in the pipeline's code, which every task finds at
// the same path, stays the path it is; a link the stage made into the code
// is no input the task was given, so the confinement refuses it as any
// other path outside the task.
func TestCodeRootsKeepPathsNotLinks(t *testing.T) {
	code := t.TempDir()
	shipped := writeSelfNamed(t, filepath.Join(code, "stages", "ship", "shipped.txt"))

	roots, err := shim.NewRoots()
	if err != nil {
		t.Fatal(err)
	}

	if roots, err = roots.WithCode(code); err != nil {
		t.Fatal(err)
	}

	task := filepath.Join(t.TempDir(), "task")

	link := filepath.Join(task, "main", "files", "linked.txt")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(shipped, link); err != nil {
		t.Fatal(err)
	}

	conf, err := shim.ConfineStage(task, "main", true, roots)
	if err != nil {
		t.Fatal(err)
	}

	opts := shim.Opts{Confine: conf, Roots: roots}

	bundle := filepath.Join(t.TempDir(), "outs")
	writeBundle(t, bundle, map[string]any{"out": shipped}, fields(t, "out file"), opts)

	if got := readPayload(t, bundle)["out"]; got != shipped {
		t.Errorf("the code file reads back at %v, want its path %s", got, shipped)
	}

	if _, err := shim.WriteItem(filepath.Join(t.TempDir(), "outs"), map[string]any{"out": link}, fields(t, "out file"), fileTable(t), opts, shim.Staged{}); err == nil {
		t.Error("a stage output linking into the code: nil error, want the confinement's refusal")
	}
}

// TestNewRootsUnresolvable: a root whose real path cannot be read for a
// reason other than its absence fails, rather than holding only its given
// path.
func TestNewRootsUnresolvable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a dir of mode 000")
	}

	locked := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(locked, 0o000); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	if _, err := shim.NewRoots(filepath.Join(locked, "ref")); err == nil {
		t.Error("NewRoots under a dir of mode 000: nil error, want the resolve failure")
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
