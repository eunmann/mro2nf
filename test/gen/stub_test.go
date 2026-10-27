package gen

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// stubDriver runs one stub phase outside Martian: it loads the stage module
// with a stand-in martian module whose make_path writes under the given files
// dir, and prints the phase's result (split's stage defs, main's outs).
const stubDriver = `
import importlib.util, json, os, sys
stage_dir, files_dir, fake_dir, phase, args_json = sys.argv[1:6]
sys.path.insert(0, fake_dir)
import martian
martian.FILES = files_dir
spec = importlib.util.spec_from_file_location("stub", os.path.join(stage_dir, "__init__.py"))
mod = importlib.util.module_from_spec(spec)
spec.loader.exec_module(mod)

class Rec(object):
    def __init__(self, d):
        self.slots = list(d)
        for k, v in d.items():
            setattr(self, k, v)
    def items(self):
        return {k: getattr(self, k) for k in self.slots}

given = json.loads(args_json)
outs = Rec({name: None for name, _ in mod.SPEC["out"] + mod.SPEC["chunk_out"]})
if phase == "split":
    print(json.dumps(mod.split(Rec(given))))
elif phase == "join":
    defs = [Rec(d) for d in given.pop("chunk_defs")]
    chunk_outs = [Rec(o) for o in given.pop("chunk_outs")]
    mod.join(Rec(given), outs, defs, chunk_outs)
    print(json.dumps(outs.items()))
else:
    mod.main(Rec(given), outs)
    print(json.dumps(outs.items()))
`

const fakeMartian = `
import os
FILES = None
def make_path(name):
    if isinstance(name, str):
        name = name.encode()
    return os.path.join(FILES.encode(), name)
def exit(message):
    raise SystemExit("ASSERT:" + message)
`

// stubProgram is a program with one split stage whose parameters span the
// value kinds the stub reads and writes.
func stubProgram() *Program {
	s := &Struct{Name: "S0", Fields: []Param{
		{Name: "f0", Type: Type{Base: "txt"}},
		{Name: "f1", Type: Type{Base: typeInt, Map: 1}},
	}}
	st := &Stage{
		Name: "ST0",
		In:   []Param{{Name: "i0", Type: Type{Base: "txt"}}, {Name: "i1", Type: Type{Base: typeFloat, Arr: 1}}},
		Out: []Param{
			{Name: seenOut, Type: Type{Base: typeString}},
			{Name: "o0", Type: Type{Base: "S0", Arr: 1}},
			{Name: "o1", Type: Type{Base: typePath}},
			{Name: "o2", Type: Type{Base: typeBool}},
			{Name: "o3", Type: Type{Base: typeString, Map: 2}},
		},
		Split:    true,
		ChunkIn:  []Param{{Name: "c0", Type: Type{Base: "S0"}}},
		ChunkOut: []Param{{Name: "d0", Type: Type{Base: typeFile, Arr: 2}}},
	}

	return &Program{
		FileTypes: []string{"txt"},
		Structs:   []*Struct{s},
		Stages:    []*Stage{st},
		Pipelines: []*Pipeline{{Name: "TOP", Calls: []*Call{{Callee: "ST0", ID: "ST0"}}}},
	}
}

type stubRunner struct {
	t        *testing.T
	py       string
	stageDir string
	fakeDir  string
}

func newStubRunner(t *testing.T, p *Program) stubRunner {
	t.Helper()

	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found; the stub runs under the e2e suites")
	}

	dir := t.TempDir()
	if err := p.WriteFixture(dir); err != nil {
		t.Fatal(err)
	}

	fake := t.TempDir()
	if err := os.WriteFile(filepath.Join(fake, "martian.py"), []byte(fakeMartian), 0o644); err != nil {
		t.Fatal(err)
	}

	return stubRunner{t: t, py: py, stageDir: filepath.Join(dir, StageDir(p.Stages[0].Name)), fakeDir: fake}
}

// run runs phase with args in a fresh files dir and returns the result with
// that dir's path replaced by <files>, plus the files dir.
func (r stubRunner) run(phase string, args map[string]any) (string, string) {
	r.t.Helper()

	files := r.t.TempDir()

	data, err := json.Marshal(args)
	if err != nil {
		r.t.Fatal(err)
	}

	cmd := exec.Command(r.py, "-c", stubDriver, r.stageDir, files, r.fakeDir, phase, string(data))

	var stderr bytes.Buffer

	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		r.t.Fatalf("stub %s: %v\n%s", phase, err, stderr.String())
	}

	return strings.ReplaceAll(string(out), files, "<files>"), files
}

// TestStubIsAPureFunctionOfItsInputs checks two runs on the same inputs give
// the same outputs and files, that a file input counts by its basename and
// content rather than its path, and that changing an input changes the record.
func TestStubIsAPureFunctionOfItsInputs(t *testing.T) {
	r := newStubRunner(t, stubProgram())

	in1, in2 := filepath.Join(t.TempDir(), "x.txt"), filepath.Join(t.TempDir(), "x.txt")
	for _, f := range []string{in1, in2} {
		if err := os.WriteFile(f, []byte("same"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	args := func(file string, f float64) map[string]any {
		return map[string]any{"i0": file, "i1": []any{f, 1}, "c0": map[string]any{"f0": nil, "f1": map[string]any{"a": 1}}}
	}

	first, filesA := r.run("main", args(in1, 2.5))
	second, filesB := r.run("main", args(in2, 2.5))

	if first != second {
		t.Errorf("same inputs, different outputs:\n%s\n%s", first, second)
	}

	if diff := cmp.Diff(dirContents(t, filesA), dirContents(t, filesB)); diff != "" {
		t.Errorf("same inputs, different files (-first +second):\n%s", diff)
	}

	if changed, _ := r.run("main", args(in1, 3)); changed == first {
		t.Error("a different float input gave the same outputs")
	}
}

// TestStubUnseededDrawsAreUnchanged pins what an unseeded stub draws, the
// sha256 of its split and main results on fixed inputs: the generative
// differential's programs and the fixtures shrunk from them draw these
// values, which a seed (TestStubSeedRedraws) must leave as they were.
func TestStubUnseededDrawsAreUnchanged(t *testing.T) {
	r := newStubRunner(t, stubProgram())
	args := map[string]any{"i0": nil, "i1": []any{2.5, 1}, "c0": map[string]any{"f0": nil, "f1": map[string]any{"a": 1}}}

	for phase, want := range map[string]string{
		"split": "a801892eaecef0a1cee88e873596db981595eb1dc4ed5da491376987900435b2",
		"main":  "4b2e4deaa8905dc80a3023706c65e04905bda14daca2018d8d197420fce601e0",
	} {
		out, _ := r.run(phase, args)
		if got := fmt.Sprintf("%x", sha256.Sum256([]byte(out))); got != want {
			t.Errorf("unseeded %s draws sha256 %s, want %s:\n%s", phase, got, want, out)
		}
	}
}

// TestStubSeedRedraws checks a seed changes what the stub draws from the
// same inputs, and the same seed draws the same.
func TestStubSeedRedraws(t *testing.T) {
	unseeded := newStubRunner(t, stubProgram())
	seeded := newStubRunner(t, stubProgram())
	setSpecSeed(t, seeded.stageDir, 7)

	args := map[string]any{"i0": nil, "i1": []any{2.5, 1}}

	base, _ := unseeded.run("main", args)
	first, _ := seeded.run("main", args)
	second, _ := seeded.run("main", args)

	if first != second {
		t.Errorf("seed 7 drew differently from the same inputs:\n%s\n%s", first, second)
	}

	if first == base {
		t.Errorf("seed 7 drew what the unseeded stub draws:\n%s", first)
	}
}

// setSpecSeed sets seed in the spec.json of the stub in stageDir.
func setSpecSeed(t *testing.T, stageDir string, seed uint64) {
	t.Helper()

	path := filepath.Join(stageDir, "spec.json")

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var spec stubSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}

	spec.Seed = seed

	if err := writeStub(stageDir, spec); err != nil {
		t.Fatal(err)
	}
}

// TestStubOutputsConform checks the stub's outputs and chunk definitions have
// the declared types, as mrp's output validation requires.
func TestStubOutputsConform(t *testing.T) {
	p := stubProgram()
	st := p.Stages[0]
	r := newStubRunner(t, p)

	for _, f := range []float64{0.5, 1e6, -2} {
		args := map[string]any{"i0": nil, "i1": []any{f}}

		defs, _ := r.run("split", args)

		var sd struct {
			Chunks []map[string]any `json:"chunks"`
		}

		decodeInto(t, defs, &sd)

		chunkOuts := make([]any, 0, len(sd.Chunks))

		for _, c := range sd.Chunks {
			checkParams(t, p, st.ChunkIn, c, "")

			chunkArgs := map[string]any{"i0": nil, "i1": []any{f}, "c0": c["c0"]}
			outs, files := r.run("main", chunkArgs)

			var o map[string]any

			decodeInto(t, outs, &o)
			checkParams(t, p, st.ChunkOut, o, files)
			chunkOuts = append(chunkOuts, o)
		}

		args["chunk_defs"], args["chunk_outs"] = sd.Chunks, chunkOuts
		outs, files := r.run("join", args)

		var o map[string]any

		decodeInto(t, outs, &o)
		checkParams(t, p, st.Out, o, files)
	}
}

// TestStubSplitNamesChunkFilesApart checks the chunks of one split, which all
// make their files in the split's one files dir, never pick the same name.
func TestStubSplitNamesChunkFilesApart(t *testing.T) {
	p := stubProgram()
	p.Stages[0].ChunkIn = []Param{{Name: "c0", Type: Type{Base: typePath, Arr: 1}}}
	r := newStubRunner(t, p)

	for i := range 100 {
		defs, _ := r.run("split", map[string]any{"i0": nil, "i1": []any{float64(i)}})

		var sd struct {
			Chunks []struct {
				C0 []any `json:"c0"`
			} `json:"chunks"`
		}

		decodeInto(t, defs, &sd)

		seen := map[any]bool{}

		for _, c := range sd.Chunks {
			for _, path := range c.C0 {
				if path != nil && seen[path] {
					t.Fatalf("split %d: two chunks share %v:\n%s", i, path, defs)
				}

				seen[path] = true
			}
		}
	}
}

func decodeInto(t *testing.T, s string, v any) {
	t.Helper()

	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()

	if err := dec.Decode(v); err != nil {
		t.Fatalf("decode %s: %v", s, err)
	}
}

func checkParams(t *testing.T, p *Program, params []Param, values map[string]any, files string) {
	t.Helper()

	for _, q := range params {
		v, ok := values[q.Name]
		if !ok {
			t.Errorf("%s missing from %v", q.Name, values)

			continue
		}

		if err := p.conforms(q.Type, v, files); err != nil {
			t.Errorf("%s %s = %v: %v", q.Type, q.Name, v, err)
		}
	}
}

var errNonconforming = errors.New("value does not have its declared type")

// conforms checks v, decoded with UseNumber, is a value of type t; a file
// must exist under files, where the stub wrote it.
func (p *Program) conforms(t Type, v any, files string) error {
	switch {
	case v == nil:
		return nil
	case t.Arr > 0 || t.Map > 0:
		return p.conformsCollection(t, v, files)
	case t.Base == typePath || p.isFileBase(t.Base):
		return conformsFile(v, files)
	case p.isStruct(t.Base):
		return p.conformsStruct(t.Base, v, files)
	default:
		return conformsScalar(t.Base, v)
	}
}

func conformsScalar(base string, v any) error {
	ok := false

	switch base {
	case typeInt, typeFloat:
		n, isNum := v.(json.Number)
		ok = isNum && (base == typeFloat || !strings.ContainsAny(n.String(), ".eE"))
	case typeString:
		_, ok = v.(string)
	case typeBool:
		_, ok = v.(bool)
	}

	if !ok {
		return fmt.Errorf("%w: %T %v for %s", errNonconforming, v, v, base)
	}

	return nil
}

func (p *Program) conformsCollection(t Type, v any, files string) error {
	var elems []any

	switch x := v.(type) {
	case []any:
		if t.Arr == 0 {
			return fmt.Errorf("%w: an array for %s", errNonconforming, t)
		}

		elems = x
	case map[string]any:
		if t.Arr > 0 {
			return fmt.Errorf("%w: a map for %s", errNonconforming, t)
		}

		for _, e := range x {
			elems = append(elems, e)
		}
	default:
		return fmt.Errorf("%w: %T for %s", errNonconforming, v, t)
	}

	for _, e := range elems {
		if err := p.conforms(t.Elem(), e, files); err != nil {
			return err
		}
	}

	return nil
}

func (p *Program) conformsStruct(base string, v any, files string) error {
	fields, _ := p.fields(base)

	obj, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf("%w: %T for struct %s", errNonconforming, v, base)
	}

	for _, f := range fields {
		fv, present := obj[f.Name]
		if !present {
			return fmt.Errorf("%w: struct %s lacks %s", errNonconforming, base, f.Name)
		}

		if err := p.conforms(f.Type, fv, files); err != nil {
			return err
		}
	}

	return nil
}

func conformsFile(v any, files string) error {
	s, ok := v.(string)
	if !ok {
		return fmt.Errorf("%w: %T for a file", errNonconforming, v)
	}

	path := strings.Replace(s, "<files>", files, 1)
	if _, err := os.Stat(path); files != "" && err != nil {
		return fmt.Errorf("%w: %s: %w", errNonconforming, s, err)
	}

	return nil
}

func dirContents(t *testing.T, dir string) map[string]string {
	t.Helper()

	out := map[string]string{}

	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(dir, path)
		if err != nil || d.IsDir() {
			out[rel+"/"] = ""

			return err
		}

		data, err := os.ReadFile(path)
		out[rel] = strings.ReplaceAll(string(data), dir, "<files>")

		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	return out
}
