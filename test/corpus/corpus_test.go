package corpus

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

const vendoredDir = "../../testdata/corpus"

// writeTree writes files, by slash path, below a new directory and returns
// it.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()

	root := t.TempDir()

	for name, body := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	return root
}

type verdict struct {
	Name     string
	Rejected string
}

// verdicts are entries' names with the part of each refusal a case names:
// the substring of Martian's error want holds for it, else "".
func verdicts(entries []Entry, want map[string]string) []verdict {
	out := make([]verdict, 0, len(entries))

	for _, e := range entries {
		v := verdict{Name: e.Name}
		if e.Rejected != nil {
			v.Rejected = e.Rejected.Error()
			if w, ok := want[e.Name]; ok && strings.Contains(v.Rejected, w) {
				v.Rejected = w
			}
		}

		out = append(out, v)
	}

	return out
}

// TestLoad: a file holding a call is its own invocation; a file without
// one yields an invocation of each pipeline and stage it declares, not of
// those it includes; a file Martian refuses, and an invocation whose null
// input Martian refuses to bind to a disable, are recorded with Martian's
// error.
func TestLoad(t *testing.T) {
	stages := "stage S(\n    in  int    n,\n    in  bool[] bs,\n    out int    y,\n    src py     \"s\",\n)\n"
	root := writeTree(t, map[string]string{
		"sub/stages.mro": stages,
		"call.mro":       "@include \"sub/stages.mro\"\n\ncall S(\n    n  = 1,\n    bs = null,\n)\n",
		"decl.mro": "@include \"sub/stages.mro\"\n\npipeline P(\n    in  bool   b,\n    in  int    n,\n    in  bool[] bs,\n    out int    y,\n)\n" +
			"{\n    call S(\n        n  = self.n,\n        bs = self.bs,\n    ) using (\n        disabled = self.b,\n    )\n\n    return (\n        y = S.y,\n    )\n}\n",
		"gated.mro": "@include \"sub/stages.mro\"\n\nstruct FLAGS(\n    bool off,\n)\n\npipeline Q(\n    in  FLAGS f,\n)\n" +
			"{\n    call S(\n        n  = 1,\n        bs = null,\n    ) using (\n        disabled = self.f.off,\n    )\n\n    return ()\n}\n",
		"bad.mro": "stage (\n",
	})

	entries, err := Load(t.TempDir(), []Source{{Name: "t", Roots: []string{root}}})
	if err != nil {
		t.Fatal(err)
	}

	refusals := map[string]string{"t/bad.mro": "compile:", "t/gated.mro/Q": "disabled cannot be bound to a null value"}
	want := []verdict{
		{Name: "t/bad.mro", Rejected: "compile:"},
		{Name: "t/call.mro"},
		{Name: "t/decl.mro/P"},
		{Name: "t/gated.mro/Q", Rejected: refusals["t/gated.mro/Q"]},
		{Name: "t/sub/stages.mro/S"},
	}

	if d := cmp.Diff(want, verdicts(entries, refusals)); d != "" {
		t.Errorf("entries (-want +got):\n%s", d)
	}

	if len(entries) != len(want) {
		t.FailNow()
	}

	if entries[1].Mro != filepath.Join(root, "call.mro") {
		t.Errorf("the call's invocation is %s, want the file itself", entries[1].Mro)
	}

	raw, err := os.ReadFile(entries[2].Mro)
	if err != nil {
		t.Fatal(err)
	}

	inv := "@include \"" + filepath.Join(root, "decl.mro") + "\"\n\ncall P(\n    b = false,\n    n = null,\n    bs = null,\n)\n"
	if d := cmp.Diff(inv, string(raw)); d != "" {
		t.Errorf("P's invocation (-want +got):\n%s", d)
	}

	for _, e := range entries {
		if d := cmp.Diff([]string{root}, e.MroPath); d != "" {
			t.Errorf("%s include path (-want +got):\n%s", e.Name, d)
		}
	}
}

// TestLoadRefusesSlugCollisions: two entries whose names differ only in
// runes Slug replaces would share an invocation directory and a lint
// project.
func TestLoadRefusesSlugCollisions(t *testing.T) {
	stage := "stage S(\n    in  int n,\n    src py \"s\",\n)\n"
	root := writeTree(t, map[string]string{"a-b.mro": stage, "a_b.mro": stage})

	if _, err := Load(t.TempDir(), []Source{{Name: "t", Roots: []string{root}}}); !errors.Is(err, errSlugCollision) {
		t.Errorf("Load = %v, want %v", err, errSlugCollision)
	}
}

// TestExternal: Env is an MROPATH; empty elements name no root, a missing
// or relative root fails loudly, and the entries are named below the roots' common
// directory, so two roots holding one file name stay apart.
func TestExternal(t *testing.T) {
	stage := "stage S(\n    in  int n,\n    src py  \"s\",\n)\n"
	tree := writeTree(t, map[string]string{"x/mro/s.mro": stage, "x/lib/mro/s.mro": stage})
	mropath := filepath.Join(tree, "x", "mro") + string(filepath.ListSeparator) + string(filepath.ListSeparator) + filepath.Join(tree, "x", "lib", "mro")

	t.Setenv(Env, mropath)

	srcs, err := Sources(vendoredDir)
	if err != nil {
		t.Fatal(err)
	}

	ext := srcs[len(srcs)-1]
	if d := cmp.Diff(Source{Name: ExternalSource, Roots: []string{filepath.Join(tree, "x", "mro"), filepath.Join(tree, "x", "lib", "mro")}}, ext); d != "" {
		t.Errorf("external source (-want +got):\n%s", d)
	}

	entries, err := Load(t.TempDir(), []Source{ext})
	if err != nil {
		t.Fatal(err)
	}

	want := []verdict{{Name: "external/lib/mro/s.mro/S"}, {Name: "external/mro/s.mro/S"}}
	if d := cmp.Diff(want, verdicts(entries, nil)); d != "" {
		t.Errorf("entries (-want +got):\n%s", d)
	}

	if _, err := External(filepath.Join(tree, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("External of a missing root = %v, want %v", err, os.ErrNotExist)
	}

	if _, err := External(string(filepath.ListSeparator)); !errors.Is(err, errNoRoots) {
		t.Errorf("External of no root = %v, want %v", err, errNoRoots)
	}

	if _, err := External("mro"); !errors.Is(err, errRelativeRoot) {
		t.Errorf("External of a relative root = %v, want %v", err, errRelativeRoot)
	}
}

func TestSlug(t *testing.T) {
	if got, want := Slug("martian-rust/a.mro/SUM_SQ2"), "martian_rust_a_mro_SUM_SQ2"; got != want {
		t.Errorf("Slug = %q, want %q", got, want)
	}
}
