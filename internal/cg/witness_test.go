package cg

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// entryFiles lists every fixture invocation: each testdata fixture's
// pipeline.mro and every bench invocation.
func entryFiles(t *testing.T) []string {
	t.Helper()

	files, err := filepath.Glob(filepath.Join("..", "..", "testdata", "*", "pipeline.mro"))
	if err != nil {
		t.Fatal(err)
	}

	bench, err := filepath.Glob(filepath.Join("..", "..", "bench", "*", "pipeline*.mro"))
	if err != nil {
		t.Fatal(err)
	}

	return append(files, bench...)
}

// TestWitnessCoversLiteral is the acceptance check of the witness
// transpile: for every fixture, each stage node of the literal invocation's
// call graph is a node of the witness's, forked on the same roots, reading
// and gated by no more, and every fork root is fed alike.
func TestWitnessCoversLiteral(t *testing.T) {
	// mrp refuses to instantiate these literal invocations (#387): a split
	// of a runtime-gated literal collection.
	mrpRefused := []string{"rt_gate_split_reject", "rt_gate_split_reject_pass", "rt_gate_split_reject_sub"}
	checked := 0

	for _, mro := range entryFiles(t) {
		dir := filepath.Dir(mro)
		name := filepath.Base(dir)

		src, err := ReadSource(mro, []string{dir})
		if slices.Contains(mrpRefused, name) {
			if err == nil {
				t.Errorf("%s: the literal invocation compiles; mrp refuses it", name)
			}

			continue
		}

		if err != nil {
			t.Errorf("%s: %v", mro, err)

			continue
		}

		if err := checkCovered(src, dir); err != nil {
			t.Errorf("%s: %v", mro, err)

			continue
		}

		checked++
	}

	if checked < 100 {
		t.Errorf("checked %d invocations, want every fixture's", checked)
	}
}

func checkCovered(src Source, dir string) error {
	w, err := src.Witness([]string{dir})
	if err != nil {
		return err
	}

	wt, err := w.Topology()
	if err != nil {
		return err
	}

	lsrc, err := src.Rebuild(src.Invocation, []string{dir})
	if err != nil {
		return err
	}

	lit, err := Compile([]byte(lsrc), src.Name, []string{dir})
	if err != nil {
		return err
	}

	lt, err := lit.Topology()
	if err != nil {
		return err
	}

	return wt.Covers(lt)
}

// TestCoversRefuses: a call graph with a node, a read or a fork root the
// witness lacks is not covered.
func TestCoversRefuses(t *testing.T) {
	w := Topology{
		Nodes: []TopoNode{{Fqid: "P.A", Stage: "A"}, {Fqid: "P.B", Stage: "B", Roots: []string{"P.B"}, Reads: []Read{{Node: "P.A", Fields: []string{"x"}}}}},
		Roots: []TopoRoot{{Fqid: "P.B"}},
	}

	for name, r := range map[string]Topology{
		"node":  {Nodes: []TopoNode{{Fqid: "P.C", Stage: "C"}}},
		"roots": {Nodes: []TopoNode{{Fqid: "P.B", Stage: "B"}}},
		"read":  {Nodes: []TopoNode{{Fqid: "P.B", Stage: "B", Roots: []string{"P.B"}, Reads: []Read{{Node: "P.A"}}}}},
		"feed":  {Nodes: w.Nodes[1:], Roots: []TopoRoot{{Fqid: "P.B", Producer: "P.A"}}},
	} {
		if err := w.Covers(r); !errors.Is(err, errUncovered) {
			t.Errorf("%s: Covers = %v, want errUncovered", name, err)
		}
	}

	if err := w.Covers(Topology{Nodes: w.Nodes[1:], Roots: w.Roots}); err != nil {
		t.Errorf("a subset is not covered: %v", err)
	}
}

// TestSourceIncludesIgnoreWorkingDir: a source read from its entry file
// resolves the entry's includes beside it, as mrp does, and never in the
// working directory, whose unrelated lib.mro would make them ambiguous.
func TestSourceIncludesIgnoreWorkingDir(t *testing.T) {
	write := func(path, body string) {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	cwd, dir := t.TempDir(), t.TempDir()
	write(filepath.Join(cwd, "lib.mro"), "stage OTHER(\n    src py \"stages/other\",\n)\n")
	write(filepath.Join(dir, "lib.mro"), "stage COUNT(\n    in  int n,\n    out int total,\n    src py \"stages/count\",\n)\n")
	write(filepath.Join(dir, "pipeline.mro"), "@include \"lib.mro\"\n\ncall COUNT(\n    n = 1,\n)\n")
	t.Chdir(cwd)

	src, err := ReadSource(filepath.Join(dir, "pipeline.mro"), []string{dir})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := src.Witness([]string{dir}); err != nil {
		t.Errorf("witness: %v", err)
	}

	if _, err := src.Manifest([]string{dir}); err != nil {
		t.Errorf("manifest: %v", err)
	}
}

// TestReadSourceKeepsInclude: the rebuilt literal invocation is the one mrp
// runs, its call after the entry file's declarations, and records the
// include mrp records (include_test: "pipeline.mro").
func TestReadSourceKeepsInclude(t *testing.T) {
	dir := filepath.Join("..", "..", "testdata", "include_test")

	src, err := ReadSource(filepath.Join(dir, "pipeline.mro"), []string{dir})
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(src.Decls, "call IP(") || !strings.Contains(src.Decls, `@include "stages.mro"`) {
		t.Errorf("declarations hold the call or lose the include:\n%s", src.Decls)
	}

	text, err := src.Rebuild(src.Invocation, []string{dir})
	if err != nil {
		t.Fatal(err)
	}

	g, err := Compile([]byte(text), src.Name, []string{dir})
	if err != nil {
		t.Fatal(err)
	}

	inv, err := g.Invocation()
	if err != nil {
		t.Fatal(err)
	}

	if inv.Include != "pipeline.mro" || string(inv.Args["a"]) != "2" || string(inv.Args["b"]) != "3" {
		t.Errorf("invocation %+v, mrp records include pipeline.mro with a = 2, b = 3", inv)
	}

	if _, err := os.Stat(filepath.Join(dir, "pipeline.mro")); err != nil {
		t.Fatal(err)
	}
}
