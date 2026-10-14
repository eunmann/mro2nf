package types_test

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/types"
	"github.com/google/go-cmp/cmp"
	"github.com/martian-lang/martian/martian/syntax"
)

// typesSrc declares the types the walk tests resolve against: user
// filetypes, structs with and without file fields, and a stage whose
// outputs Martian registers as the struct MAKE (structFromCallable).
const typesSrc = `
filetype txt;
filetype bam;
filetype fastq.gz;

struct Cfg(
    file ref,
    int  n,
)

struct Outer(
    Cfg  inner,
    file log,
)

struct Notes(
    string note,
    map    extra,
)

struct Num(
    float x,
    int   n,
)

stage MAKE(
    in  float scale,
    out file  data,
    out int   count,
    src py    "make.py",
) split (
    in  int   part,
    out txt   piece,
)

pipeline TOP(
    in  float k,
    out MAKE  made,
)
{
    call MAKE(
        scale = self.k,
    )

    return (
        made = MAKE,
    )
}
`

// compile compiles src as mrp compiles an invocation's declarations.
func compile(t *testing.T, src string) *syntax.Ast {
	t.Helper()

	_, _, ast, err := syntax.ParseSourceBytes([]byte(src), "types.mro", nil, false)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	return ast
}

func newTable(t *testing.T) *types.Table {
	t.Helper()

	return types.NewTable(&compile(t, typesSrc).TypeTable)
}

// fields declares each "name type" pair, the type as an .mro spells it, as a
// member of the object the walk descends.
func fields(t *testing.T, decls ...string) []*syntax.StructMember {
	t.Helper()

	out := make([]*syntax.StructMember, len(decls))

	for i, d := range decls {
		name, tname, _ := strings.Cut(d, " ")

		var id syntax.TypeId
		if err := id.UnmarshalText([]byte(tname)); err != nil {
			t.Fatalf("type of %s: %v", d, err)
		}

		out[i] = &syntax.StructMember{Id: name, Tname: id}
	}

	return out
}

// upper rewrites a path to its uppercase form, so tests can assert exactly which
// leaves the walk reached.
func upper(s string) (string, error) { return strings.ToUpper(s), nil }

// collector records every file leaf the walk visits (returning it unchanged).
func collector() (types.Transform, *[]string) {
	var seen []string

	fn := func(s string) (string, error) {
		seen = append(seen, s)

		return s, nil
	}

	return fn, &seen
}

// TestApplySkipLeaf guards bug 7: a Transform returning ErrSkipLeaf resolves that
// one leaf to null and continues the walk, rather than aborting it. publish uses
// this for a declared output file the pipeline legitimately never produced —
// matching Martian, which Lstats each published file and writes null when absent
// (core/post_process.go), instead of failing the whole run.
func TestApplySkipLeaf(t *testing.T) {
	fn := func(s string) (string, error) {
		if s == "skipme" {
			return "", types.ErrSkipLeaf
		}

		return "X:" + s, nil
	}

	vals := map[string]any{"a": "skipme", "b": "keep"}

	got, err := newTable(t).Apply(fields(t, "a file", "b file"), vals, fn)
	if err != nil {
		t.Fatalf("ErrSkipLeaf must not abort the walk: %v", err)
	}

	want := map[string]any{"a": nil, "b": "X:keep"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Apply with ErrSkipLeaf mismatch (-want +got):\n%s", diff)
	}
}

type leafCase struct {
	name string
	decl string
	val  any
	want []string // file leaves, sorted
}

// leafShapeCases is a long-by-design table of file-leaf shapes. A leaf is a
// string at a type whose Martian file kind is a file (file, path, a user
// filetype) or a directory (a struct holding files); a string or an untyped
// map may hold paths but is no leaf (syntax KindMayContainPaths).
func leafShapeCases() []leafCase {
	return []leafCase{
		{name: "single file", decl: "f file", val: "a.txt", want: []string{"a.txt"}},
		{name: "path", decl: "p path", val: "d/", want: []string{"d/"}},
		{name: "user filetype", decl: "b bam", val: "x.bam", want: []string{"x.bam"}},
		{name: "dotted user filetype", decl: "r fastq.gz", val: "r.fastq.gz", want: []string{"r.fastq.gz"}},
		{name: "file array", decl: "fs file[]", val: []any{"a", "b"}, want: []string{"a", "b"}},
		{name: "file matrix", decl: "fm txt[][]", val: []any{[]any{"a", "b"}, []any{"c"}}, want: []string{"a", "b", "c"}},
		{name: "map of file", decl: "m map<file>", val: map[string]any{"k1": "x", "k2": "y"}, want: []string{"x", "y"}},
		{name: "array of file map", decl: "a map<file>[]", val: []any{map[string]any{"k": "p"}, map[string]any{"k": "q"}}, want: []string{"p", "q"}},
		{name: "map of file array", decl: "m map<file[]>", val: map[string]any{"k": []any{"p", "q"}}, want: []string{"p", "q"}},
		{name: "map of file matrix", decl: "m map<file[][]>", val: map[string]any{"k": []any{[]any{"a", "b"}, []any{"c"}}}, want: []string{"a", "b", "c"}},
		{name: "non-file primitive untouched", decl: "n int", val: json.Number("5")},
		{name: "string is no leaf", decl: "s string", val: "a.txt"},
		{name: "untyped map is not walked", decl: "um map", val: map[string]any{"k": "a.txt"}},
		{name: "struct with file field", decl: "c Cfg", val: map[string]any{"ref": "r.bam", "n": json.Number("1")}, want: []string{"r.bam"}},
		// Martian marks a struct that contains files as a directory kind, so a
		// string where one is declared is a leaf.
		{name: "string at a struct holding files", decl: "c Cfg", val: "dir", want: []string{"dir"}},
		{name: "string at a struct holding no files", decl: "n Notes", val: "x"},
		{name: "struct holding no files", decl: "n Notes", val: map[string]any{"note": "a.txt", "extra": map[string]any{"k": "b.txt"}}},
		{name: "array of struct", decl: "cs Cfg[]", val: []any{
			map[string]any{"ref": "a", "n": json.Number("1")},
			map[string]any{"ref": "b", "n": json.Number("2")},
		}, want: []string{"a", "b"}},
		{name: "map of struct", decl: "cm map<Cfg>", val: map[string]any{"x": map[string]any{"ref": "a"}, "y": map[string]any{"ref": "b"}}, want: []string{"a", "b"}},
		{name: "nested struct", decl: "o Outer", val: map[string]any{
			"inner": map[string]any{"ref": "deep.txt", "n": json.Number("1")},
			"log":   "top.log",
		}, want: []string{"deep.txt", "top.log"}},
		// #173: a param typed by a callable name is the struct of the
		// callable's outputs, whose file leaves the walk must reach.
		{name: "callable output struct", decl: "made MAKE", val: map[string]any{"data": "in/f", "count": json.Number("5")}, want: []string{"in/f"}},
		{name: "null file tolerated", decl: "f file", val: nil},
	}
}

func TestApplyLeafShapes(t *testing.T) {
	tbl := newTable(t)

	for _, tc := range leafShapeCases() {
		t.Run(tc.name, func(t *testing.T) {
			fn, seen := collector()
			ps := fields(t, tc.decl)

			if _, err := tbl.Apply(ps, map[string]any{ps[0].Id: tc.val}, fn); err != nil {
				t.Fatalf("Apply: %v", err)
			}

			got := append([]string(nil), *seen...)
			sort.Strings(got)

			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("leaves mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestApplyCallableOutputs walks a pipeline's outs, the struct Martian makes
// of its outputs, through an output typed by a stage: the stage's file
// output is rewritten in place and its other outputs kept.
func TestApplyCallableOutputs(t *testing.T) {
	ast := compile(t, typesSrc)

	outs, err := types.Params(ast.Callables.Table["TOP"], types.RoleOut)
	if err != nil {
		t.Fatal(err)
	}

	got, err := types.NewTable(&ast.TypeTable).Apply(outs, map[string]any{
		"made": map[string]any{"data": "in/f", "count": json.Number("5")},
	}, upper)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	want := map[string]any{"made": map[string]any{"data": "IN/F", "count": json.Number("5")}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("callable output struct (-want +got):\n%s", diff)
	}
}

// TestApplyRewrites verifies the transform result is threaded back into the
// returned structure, at every nesting depth, while non-file data is preserved.
func TestApplyRewrites(t *testing.T) {
	vals := map[string]any{
		"o": map[string]any{
			"inner": map[string]any{"ref": "deep.txt", "n": json.Number("7")},
			"log":   "top.log",
		},
		"keep": "untouched",
	}

	got, err := newTable(t).Apply(fields(t, "o Outer"), vals, upper)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	want := map[string]any{
		"o": map[string]any{
			"inner": map[string]any{"ref": "DEEP.TXT", "n": json.Number("7")},
			"log":   "TOP.LOG",
		},
		"keep": "untouched",
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("rewrite mismatch (-want +got):\n%s", diff)
	}
}

// TestApplyMapDeterministic checks that a map's file leaves are visited in a
// stable (sorted-key) order so bundle layout is reproducible across runs.
func TestApplyMapDeterministic(t *testing.T) {
	tbl := newTable(t)
	vals := map[string]any{"m": map[string]any{"z": "zf", "a": "af", "m": "mf"}}
	ps := fields(t, "m map<file>")

	for range 5 {
		fn, seen := collector()
		if _, err := tbl.Apply(ps, vals, fn); err != nil {
			t.Fatalf("Apply: %v", err)
		}

		if diff := cmp.Diff([]string{"af", "mf", "zf"}, *seen); diff != "" {
			t.Errorf("visit order not sorted/stable (-want +got):\n%s", diff)
		}
	}
}

// TestApplyUnknownKeysPreserved checks that values without a declared param
// survive the walk unchanged.
func TestApplyUnknownKeysPreserved(t *testing.T) {
	vals := map[string]any{"declared": "a.txt", "extra": json.Number("9")}

	got, err := newTable(t).Apply(fields(t, "declared file"), vals, upper)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	want := map[string]any{"declared": "A.TXT", "extra": json.Number("9")}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Apply mismatch (-want +got):\n%s", diff)
	}
}

// TestWalkShapeMismatchPassthrough checks values whose runtime shape disagrees
// with the declared type pass through untouched instead of erroring — runtime
// values may legitimately be null or oddly-shaped mid-pipeline.
func TestWalkShapeMismatchPassthrough(t *testing.T) {
	calls := 0
	fn := func(p string) (string, error) {
		calls++

		return p, nil
	}

	vals := map[string]any{
		"arrAsScalar": []any{"a"},
		"numAsFile":   json.Number("7"),
		"mapAsFile":   map[string]any{"k": "v"},
		"scalarAsArr": "a",
		"mapAsArr":    map[string]any{"k": "v"},
		"arrAsMap":    []any{"v"},
	}
	ps := fields(t, "arrAsScalar txt", "numAsFile txt", "mapAsFile txt", "scalarAsArr file[]", "mapAsArr file[]", "arrAsMap map<file>")

	got, err := newTable(t).Apply(ps, vals, fn)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if calls != 0 {
		t.Errorf("transform ran %d times on mismatched shapes, want 0", calls)
	}

	if diff := cmp.Diff(vals, got); diff != "" {
		t.Errorf("mismatched shapes were rewritten (-want +got):\n%s", diff)
	}
}

// TestHasFiles: a type holds a file leaf when Apply could walk to one: a
// file, a path, a filetype, or an array, typed map or struct of one, at any
// depth; never a scalar, an untyped map, or a struct of neither.
func TestHasFiles(t *testing.T) {
	tbl := newTable(t)

	got := map[string]bool{}

	for _, p := range fields(t, "a int", "b string", "c map", "d bool", "e float[]", "f map<int>", "g Notes", "h Num[]",
		"i file", "j path", "k txt", "l txt[][]", "m map<bam>", "n Cfg", "o Outer[]", "p map<Cfg[]>") {
		has, err := tbl.HasFiles(p.Tname)
		if err != nil {
			t.Fatalf("%s: %v", p.Id, err)
		}

		got[p.Id] = has
	}

	want := map[string]bool{
		"a": false, "b": false, "c": false, "d": false, "e": false, "f": false, "g": false, "h": false,
		"i": true, "j": true, "k": true, "l": true, "m": true, "n": true, "o": true, "p": true,
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("HasFiles (-want +got):\n%s", diff)
	}

	if _, err := tbl.HasFiles(fields(t, "x nosuch")[0].Tname); !errors.Is(err, types.ErrUnknownType) {
		t.Errorf("HasFiles of an unknown type = %v, want %v", err, types.ErrUnknownType)
	}
}

// TestUnknownType checks a param whose type the type table lacks fails the
// walk loudly instead of passing its file leaves through unstaged.
func TestUnknownType(t *testing.T) {
	tbl := newTable(t)
	ps := fields(t, "x nosuch")

	if _, err := tbl.Apply(ps, map[string]any{"x": "a"}, upper); !errors.Is(err, types.ErrUnknownType) {
		t.Errorf("Apply = %v, want %v", err, types.ErrUnknownType)
	}
}
