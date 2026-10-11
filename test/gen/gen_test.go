package gen

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/martian-lang/martian/martian/syntax"
)

// TestGenerateIsDeterministic checks a seed always gives the same program
// and the same fixture files.
func TestGenerateIsDeterministic(t *testing.T) {
	for seed := uint64(1); seed <= 25; seed++ {
		a, b := mustGenerate(t, seed), mustGenerate(t, seed)

		if diff := cmp.Diff(fixtureFiles(t, a), fixtureFiles(t, b)); diff != "" {
			t.Fatalf("seed %d: two generations differ (-first +second):\n%s", seed, diff)
		}
	}
}

// TestGenerateSeedsDiffer checks the seed drives the program.
func TestGenerateSeedsDiffer(t *testing.T) {
	seen := map[string]uint64{}

	for seed := uint64(1); seed <= 50; seed++ {
		_, src, _ := strings.Cut(mustGenerate(t, seed).MRO(), "\n")
		if prev, dup := seen[src]; dup {
			t.Errorf("seeds %d and %d give the same program", prev, seed)
		}

		seen[src] = seed
	}
}

// TestGeneratedProgramsCompile checks every program Generate returns
// compiles under Martian with its stage code present, and that construction
// mostly gets it right the first time.
func TestGeneratedProgramsCompile(t *testing.T) {
	retried := 0

	for seed := uint64(1); seed <= 300; seed++ {
		p := mustGenerate(t, seed)
		if p.Attempt > 0 {
			retried++
		}

		if seed%30 == 0 {
			dir := t.TempDir()
			if err := p.WriteFixture(dir); err != nil {
				t.Fatal(err)
			}

			if _, _, _, err := syntax.Compile(filepath.Join(dir, "pipeline.mro"), []string{dir}, true); err != nil {
				t.Errorf("seed %d: written fixture does not compile: %v", seed, err)
			}
		}
	}

	if retried > 15 {
		t.Errorf("%d of 300 seeds needed more than one attempt; the generator builds invalid programs", retried)
	}
}

// TestGrammarCoverage checks the seeds reach every construct and corpus value
// the differential is meant to explore.
func TestGrammarCoverage(t *testing.T) {
	want := []string{
		"type:struct", "type:struct-of-struct", "type:struct-with-file", "type:map", "type:array",
		"type:array-2d", "type:filetype", "type:file", "type:path",
		"stage:split", "stage:fail", "pipeline:sub", "call:sub", "call:alias",
		"call:map-array", "call:map-map", "call:map-zip", "call:map-disabled", "call:map-split-stage",
		"call:map-sub", "call:map-nested", "disabled:self", "disabled:call",
		"bind:projection-through-array", "bind:projection-through-map", "bind:wildcard-self",
		"bind:wildcard-call", "bind:fanin-array", "bind:fanin-map", "bind:whole-ref",
		"bind:whole-ref-mapped", "bind:whole-ref-disabled", "bind:int-as-float",
		"value:null", "value:empty-array", "value:empty-map", "value:float-at-least-1e6",
		"value:float-at-most-1e-6", "value:negative-zero", "value:int-beyond-2^53", "value:marker-string",
		"key:empty", "key:tilde", "key:contains-tilde", "key:newline", "key:unicode", "key:numeric",
	}

	got := map[string]bool{}

	for seed := uint64(1); seed <= 200; seed++ {
		maps.Copy(got, Features(mustGenerate(t, seed)))
	}

	for _, f := range want {
		if !got[f] {
			t.Errorf("no seed in 1..200 generates %s", f)
		}
	}
}

// TestQuoteRoundTrips checks every corpus string survives MRO quoting as
// Martian's parser reads it.
func TestQuoteRoundTrips(t *testing.T) {
	var parser syntax.Parser

	for _, s := range slices.Concat(stringValues(), mapKeys(), []string{"\x01ctl\x7f", "\r"}) {
		exp, err := parser.ParseValExp([]byte(quote(s)))
		if err != nil {
			t.Fatalf("parse %s: %v", quote(s), err)
		}

		str, ok := exp.(*syntax.StringExp)
		if !ok || str.Value != s {
			t.Errorf("%q quoted as %s parses to %#v", s, quote(s), exp)
		}
	}
}

func TestTypeShapes(t *testing.T) {
	s := Type{Base: "S"}
	tests := []struct {
		name string
		got  any
		want any
	}{
		{"map of arrays renders inner dims", Type{Base: "int", Map: 2, Arr: 1}.String(), "map<int[]>[]"},
		{"array elem", Type{Base: "int", Arr: 2}.Elem(), Type{Base: "int", Arr: 1}},
		{"map elem", Type{Base: "int", Map: 2}.Elem(), Type{Base: "int", Arr: 1}},
		{"map of map is no type", liftOK(Type{Base: "int", Map: 1}, forkMap), false},
		{"map fork of an array", must(Type{Base: "int", Arr: 1}.lift(forkMap)), Type{Base: "int", Map: 2}},
		{"projection through an array", must(s.ArrayOf().project(Type{Base: "int", Map: 1})), Type{Base: "int", Arr: 1, Map: 1}},
		{"projection through a map", must(Type{Base: "S", Map: 1}.project(Type{Base: "int", Arr: 1})), Type{Base: "int", Map: 2}},
		{"no projection through map<S[]>", projectOK(Type{Base: "S", Map: 2}, Type{Base: "int"}), false},
	}

	for _, tc := range tests {
		if diff := cmp.Diff(tc.want, tc.got); diff != "" {
			t.Errorf("%s (-want +got):\n%s", tc.name, diff)
		}
	}
}

func liftOK(t Type, m forkMode) bool {
	_, ok := t.lift(m)

	return ok
}

func projectOK(t, f Type) bool {
	_, ok := t.project(f)

	return ok
}

func must(t Type, ok bool) Type {
	if !ok {
		return Type{Base: "<none>"}
	}

	return t
}

// TestWriteFixtureLayout checks the fixture holds the program and, per stage,
// the stub and a spec naming every struct a value of the stage may carry.
func TestWriteFixtureLayout(t *testing.T) {
	p := mustGenerate(t, 7)
	files := fixtureFiles(t, p)

	if files["pipeline.mro"] != p.MRO() {
		t.Error("pipeline.mro is not the program's MRO")
	}

	for _, s := range p.Stages {
		dir := StageDir(s.Name)
		if files[dir+"/__init__.py"] != string(stubCode) {
			t.Errorf("%s has no stub", dir)
		}

		if _, ok := files[dir+"/spec.json"]; !ok {
			t.Errorf("%s has no spec", dir)
		}
	}

	if got, want := len(files), 1+2*len(p.Stages); got != want {
		t.Errorf("fixture has %d files, want %d: %v", got, want, slices.Sorted(maps.Keys(files)))
	}
}

func mustGenerate(t *testing.T, seed uint64) *Program {
	t.Helper()

	p, err := Generate(seed)
	if err != nil {
		t.Fatal(err)
	}

	return p
}

// fixtureFiles writes p's fixture and reads it back, keyed by slash path.
func fixtureFiles(t *testing.T, p *Program) map[string]string {
	t.Helper()

	dir := t.TempDir()
	if err := p.WriteFixture(dir); err != nil {
		t.Fatal(err)
	}

	files := map[string]string{}

	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(dir, path)
		files[filepath.ToSlash(rel)] = string(data)

		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	return files
}
