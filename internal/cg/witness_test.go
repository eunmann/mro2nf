package cg

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/test/corpus"
	"github.com/martian-lang/martian/martian/syntax"
)

// sweepEntry is an invocation the fixture sweeps run.
type sweepEntry struct {
	// name keys the sweeps' exemptions: a fixture's directory, or a corpus
	// entry's name below "corpus/".
	name    string
	label   string
	mro     string
	mroPath []string
}

// sweepEntries are every testdata fixture's pipeline.mro and every bench
// invocation, each compiled with its directory as include path, and every
// corpus invocation Martian accepts (test/corpus), with its source's.
func sweepEntries(t *testing.T) []sweepEntry {
	t.Helper()

	files, err := filepath.Glob(filepath.Join("..", "..", "testdata", "*", "pipeline.mro"))
	if err != nil {
		t.Fatal(err)
	}

	bench, err := filepath.Glob(filepath.Join("..", "..", "bench", "*", "pipeline*.mro"))
	if err != nil {
		t.Fatal(err)
	}

	out := make([]sweepEntry, 0, len(files)+len(bench))

	for _, mro := range append(files, bench...) {
		dir := filepath.Dir(mro)
		out = append(out, sweepEntry{name: filepath.Base(dir), label: filepath.Base(dir) + "/" + filepath.Base(mro), mro: mro, mroPath: []string{dir}})
	}

	srcs, err := corpus.Sources(filepath.Join("..", "..", "testdata", "corpus"))
	if err != nil {
		t.Fatal(err)
	}

	entries, err := corpus.Load(t.TempDir(), srcs)
	if err != nil {
		t.Fatal(err)
	}

	for _, e := range corpus.Accepted(entries) {
		out = append(out, sweepEntry{name: "corpus/" + e.Name, label: "corpus/" + e.Name, mro: e.Mro, mroPath: e.MroPath})
	}

	return out
}

// TestWitnessCoversLiteral is the acceptance check of the witness
// transpile: for every fixture, each stage node of the literal invocation's
// call graph is a node of the witness's, forked on the same roots, reading
// and gated by no more, and every fork root is fed alike.
func TestWitnessCoversLiteral(t *testing.T) {
	checked := 0

	for _, e := range sweepEntries(t) {
		src, err := ReadSource(e.mro, e.mroPath)
		if err != nil {
			t.Errorf("%s: %v", e.label, err)

			continue
		}

		if err := checkCovered(src, e.mroPath); err != nil {
			t.Errorf("%s: %v", e.label, err)

			continue
		}

		checked++
	}

	if checked < 100 {
		t.Errorf("checked %d invocations, want every fixture's", checked)
	}
}

func checkCovered(src Source, mroPaths []string) error {
	_, wt, err := src.Witness(mroPaths)
	if err != nil {
		return err
	}

	return src.CoversLiteral(wt, mroPaths)
}

// TestCoversLiteralRefuses (#621): a source whose own call's graph a
// topology does not cover fails naming the call, which a transpile then
// refuses rather than emit a project that cannot run it.
func TestCoversLiteralRefuses(t *testing.T) {
	dir := filepath.Join("..", "..", "testdata", "witness_flag_disables")

	src, err := ReadSource(filepath.Join(dir, "pipeline.mro"), []string{dir})
	if err != nil {
		t.Fatal(err)
	}

	err = src.CoversLiteral(Topology{}, []string{dir})
	if !errors.Is(err, errUncovered) || !strings.Contains(err.Error(), "the call of pipeline.mro") {
		t.Errorf("CoversLiteral of an empty topology = %v, want errUncovered naming the call", err)
	}

	_, wt, err := src.Witness([]string{dir})
	if err != nil {
		t.Fatal(err)
	}

	if err := src.CoversLiteral(wt, []string{dir}); err != nil {
		t.Errorf("CoversLiteral of its witness's topology = %v", err)
	}
}

// TestCoversRefuses: a call graph with a node, a read or a fork root the
// witness lacks is not covered, nor a node forked on fewer of the witness's
// roots unless the witness's is Collapsible (#621).
func TestCoversRefuses(t *testing.T) {
	w := Topology{
		Nodes: []TopoNode{{Fqid: "P.A", Stage: "A"}, {Fqid: "P.B", Stage: "B", Roots: []string{"P.B"}, Reads: []Read{{Node: "P.A", Fields: []string{"x"}}}}},
		Roots: []TopoRoot{{Fqid: "P.B"}},
	}
	fewer := Topology{Nodes: []TopoNode{{Fqid: "P.B", Stage: "B", Reads: w.Nodes[1].Reads}}}

	for name, r := range map[string]Topology{
		"node":        {Nodes: []TopoNode{{Fqid: "P.C", Stage: "C"}}},
		"roots":       {Nodes: []TopoNode{{Fqid: "P.A", Stage: "A", Roots: []string{"P.B"}}}},
		"read":        {Nodes: []TopoNode{{Fqid: "P.B", Stage: "B", Roots: []string{"P.B"}, Reads: []Read{{Node: "P.A"}}}}},
		"feed":        {Nodes: w.Nodes[1:], Roots: []TopoRoot{{Fqid: "P.B", Producer: "P.A"}}},
		"fewer roots": fewer,
	} {
		if err := w.Covers(r); !errors.Is(err, errUncovered) {
			t.Errorf("%s: Covers = %v, want errUncovered", name, err)
		}
	}

	if err := w.Covers(Topology{Nodes: w.Nodes[1:], Roots: w.Roots}); err != nil {
		t.Errorf("a subset is not covered: %v", err)
	}

	w.Nodes[1].Collapsible = true

	if err := w.Covers(fewer); err != nil {
		t.Errorf("fewer roots of a collapsible node is not covered: %v", err)
	}
}

// TestWitnessOfANullInAMappedMap (gen_9641884889b0, #595): a map call over
// a literal map holding a null compiles, which Martian's call-graph builder
// failed to do before 50ad6c0 ("invalid merge kind null"), whether the
// literal invocation disables the map call or the witness enables it.
func TestWitnessOfANullInAMappedMap(t *testing.T) {
	const src = `filetype json;
stage S(in int i, out int o, src py "stages/s",)

pipeline INNER(in int[] xs, out int[] r,)
{
    map call S(i = split self.xs,)
    return (r = S.o,)
}

pipeline TOP(in bool off,)
{
    map call INNER(xs = split {"a": null, "b": []},) using (disabled = self.off,)
    return ()
}

call TOP(off = true,)
`

	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.mro")

	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := ReadSource(path, []string{dir})
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.Witness([]string{dir}); err != nil {
		t.Errorf("witness: %v", err)
	}

	if _, err := Compile([]byte(s.with(`call TOP(off = false,)`)), s.Name, []string{dir}); err != nil {
		t.Errorf("Compile of the enabled call: %v", err)
	}
}

// TestCallGraphPanicIsACompileError: a panic of Martian's call-graph
// builder, here on a call of a callable no AST the parser builds lacks, is
// the invocation's compile error, from which the witness falls back as from
// any invocation mrp refuses, not a crash of the transpile.
func TestCallGraphPanicIsACompileError(t *testing.T) {
	_, err := callGraph(&syntax.Ast{Call: &syntax.CallStm{Id: "X", DecId: "X"}}, "pipeline.mro")
	if !errors.Is(err, errCompilerPanic) {
		t.Errorf("callGraph = %v, want errCompilerPanic", err)
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

	if _, _, err := src.Witness([]string{dir}); err != nil {
		t.Errorf("witness: %v", err)
	}

	if _, _, err := src.InParams("COUNT", []string{dir}); err != nil {
		t.Errorf("entry params: %v", err)
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
