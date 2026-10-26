package cg

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/google/go-cmp/cmp"
	"github.com/martian-lang/martian/martian/core"
	"github.com/martian-lang/martian/martian/syntax"
)

// posted is what a post wrote for its readers output by output: its
// fields dir, its value records by output, and the gates it set.
type posted struct {
	fields string
	values map[string]string
	gates  string
}

// postFields posts the instance outs stand for with Gates set, and returns
// what it wrote.
func (f *fixture) postFields(outs string) posted {
	f.t.Helper()

	spec := postedSpec(f.t, f.g, outs, f.next("forks"))
	spec.Gates = f.next("gates")

	fields, values, gates := spec.Fields, spec.Values, spec.Gates
	if err := f.g.Post(spec); err != nil {
		f.t.Fatal(err)
	}

	recs := map[string]string{}

	for rec := range strings.FieldsSeq(readFile(f.t, values)) {
		field, _, _ := strings.Cut(rec, ":")
		recs[field] = rec
	}

	return posted{fields: fields, values: recs, gates: readFile(f.t, gates)}
}

func readFile(t *testing.T, path string) string {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	return string(raw)
}

// bundles are the names of the output bundles under a post's fields dir.
func bundles(t *testing.T, fields string) []string {
	t.Helper()

	dirs, err := filepath.Glob(filepath.Join(fields, "*"))
	if err != nil {
		t.Fatal(err)
	}

	out := make([]string, len(dirs))
	for i, d := range dirs {
		out[i] = filepath.Base(d)
	}

	return out
}

// TestPostFields (#536): SRC's post carries its int and bool outputs, which
// hold no file, as value records stamped with its instance, not as bundles,
// and USE, given only the y record, gets the _args mrp gave chain_gate's
// USE (CH/USE/fork0/split/_args: {"k":3,"y":4}).
func TestPostFields(t *testing.T) {
	f := loadFixture(t, "chain_gate")

	p := f.postFields(f.outs("CH.SRC", nil, `{"y":4,"flag":false}`))

	if got := bundles(t, p.fields); len(got) != 0 {
		t.Errorf("SRC's post wrote bundles %v, want none", got)
	}

	for field, want := range map[string]string{"y": `4`, "flag": `false`} {
		name, rec, err := decodeValue(p.values[field])
		if err != nil || rec.Instance == nil || rec.Instance.Node != "CH.SRC" || name != field || string(rec.Value) != want {
			t.Errorf("SRC's %s record is %s of %+v = %s (%v), want %s of CH.SRC", field, name, rec.Instance, rec.Value, err, want)
		}
	}

	_, got := f.writeArgs(ArgsSpec{Node: "CH.USE", Values: []string{p.values["y"]}})
	wantArgs(t, "CH.USE", got, `{"k":3,"y":4}`)
}

// unreadMRO reads MAKE's a in USE and its n in the pipeline outs, and no
// other output of MAKE.
const unreadMRO = `filetype txt;

stage MAKE(
    out txt a,
    out txt b,
    out int n,
    out int m,
    src py  "stages/make",
)

stage USE(
    in  txt f,
    out int size,
    src py  "stages/use",
)

pipeline P(
    out int n,
    out int size,
)
{
    call MAKE()

    call USE(
        f = MAKE.a,
    )

    return (
        n    = MAKE.n,
        size = USE.size,
    )
}

call P()
`

// TestPostSkipsUnreadOutputs (#623): a post writes only the outputs a
// reader reads, a stage's binding or the pipeline outs, as a bundle or a
// value record, so a file no task stages is not uploaded; the node's spec
// digest holds them, so a re-transpile that reads another re-runs it; a
// whole-call reference reads every output.
func TestPostSkipsUnreadOutputs(t *testing.T) {
	mro := filepath.Join(t.TempDir(), "pipeline.mro")
	if err := os.WriteFile(mro, []byte(unreadMRO), 0o644); err != nil {
		t.Fatal(err)
	}

	g, err := Load(mro, "", nil)
	if err != nil {
		t.Fatal(err)
	}

	f := &fixture{t: t, g: g, mro: mro, dir: t.TempDir()}
	src := t.TempDir()
	a, b := writeFileOf(t, filepath.Join(src, "a.txt"), "a"), writeFileOf(t, filepath.Join(src, "b.txt"), "b")

	p := f.postFields(f.outs("P.MAKE", nil, fmt.Sprintf(`{"a":%q,"b":%q,"n":1,"m":2}`, a, b)))

	if d := cmp.Diff([]string{"a"}, bundles(t, p.fields)); d != "" {
		t.Errorf("MAKE's bundles (-want +got):\n%s", d)
	}

	if d := cmp.Diff([]string{"n"}, slices.Sorted(maps.Keys(p.values))); d != "" {
		t.Errorf("MAKE's value records (-want +got):\n%s", d)
	}

	st, err := g.Stage("P.MAKE")
	if err != nil {
		t.Fatal(err)
	}

	readers, err := g.postReaders(st)
	if err != nil || !slices.Equal(readers.Outs, []string{"a", "n"}) {
		t.Errorf("MAKE's readers read %v (%v), want [a n]", readers.Outs, err)
	}

	whole := loadFixture(t, "disabled_wholeref")

	zero, err := whole.g.Stage("DW.ZERO")
	if err != nil {
		t.Fatal(err)
	}

	if posted, err := whole.g.postedOutputs(zero); err != nil || len(posted) != 1 || posted[0].Id != "v" {
		t.Errorf("a whole-call reference to ZERO reads %v (%v), want its one output v", posted, err)
	}
}

// TestPostFieldsNeedsValues: a post writes its outs field by field, so it
// needs the fields dir, the values file and the refs file, as an output it
// carries as a value, or a file a bundle names by reference, would
// otherwise reach no reader.
func TestPostFieldsNeedsValues(t *testing.T) {
	f := loadFixture(t, "chain_gate")
	src := f.outs("CH.SRC", nil, `{"y":4,"flag":false}`)

	for _, drop := range []func(*PostSpec){func(s *PostSpec) { s.Fields = "" }, func(s *PostSpec) { s.Values = "" }, func(s *PostSpec) { s.FieldRefs = "" }} {
		spec := postedSpec(t, f.g, src, f.next("forks"))
		drop(&spec)

		if err := f.g.Post(spec); !errors.Is(err, errPostDirs) {
			t.Errorf("post with fields %q, values %q and refs %q: %v, want %v", spec.Fields, spec.Values, spec.FieldRefs, err, errPostDirs)
		}
	}
}

// echoOuts are outs of marker_strings' ECHO whose values hold no file:
// strings shaped as bundle markers, floats in the forms mrp keeps as
// written, a null, HTML characters JSON escapes and members out of key
// order; and made, a file.
const echoOuts = `{"s":"@mre:file:f/L0000/made.txt","arr":["@mre:dir:x","@mre:str:y"],` +
	`"m":{"z":1.0,"a":"<&>","n":null,"e":1E6,"neg":-0.0,"u":"é "},"lit":%s,"made":%q,"fake":"@mre:file:f/L0001/fake.txt"}`

// TestPostFieldsKeepsValues (#536): a reader gets each output ECHO posts as
// a value record with the very JSON it reads from ECHO's whole outs bundle,
// and each file output, and one whose JSON passes MaxValueBytes, as a
// bundle; a value of MaxValueBytes exactly is a record.
func TestPostFieldsKeepsValues(t *testing.T) {
	f := loadFixture(t, "marker_strings")

	made := filepath.Join(t.TempDir(), "made.txt")
	if err := os.WriteFile(made, []byte("made"), 0o644); err != nil {
		t.Fatal(err)
	}

	for lit, want := range map[int][]string{MaxValueBytes - 2: {"fake", "made"}, MaxValueBytes - 1: {"fake", "lit", "made"}} {
		outs := f.outs("MARKERS.ECHO", nil, fmt.Sprintf(echoOuts, strconv.Quote(strings.Repeat("x", lit)), made))
		p := f.postFields(outs)

		if got := bundles(t, p.fields); !cmp.Equal(got, want) {
			t.Errorf("a %d-byte lit: ECHO's post wrote bundles %v, want %v", lit+2, got, want)
		}

		whole := f.storedOuts(t, "MARKERS.ECHO", []string{outs}, nil)

		recs := slices.Collect(maps.Values(p.values))

		for field, raw := range f.storedOuts(t, "MARKERS.ECHO", nil, recs) {
			if d := cmp.Diff(string(whole[field]), string(raw)); d != "" {
				t.Errorf("ECHO's %s as a value record (-bundle +record):\n%s", field, d)
			}
		}

		if len(recs)+len(want) != 6 {
			t.Errorf("a %d-byte lit: ECHO posted %d records and %d bundles, want its 6 outputs", lit+2, len(recs), len(want))
		}
	}
}

// storedOuts are node's outs at the unforked instance as a task reads them
// from the staged items in and the value records values.
func (f *fixture) storedOuts(t *testing.T, node string, in, values []string) core.LazyArgumentMap {
	t.Helper()

	s, err := f.g.stage(in, values, shim.Refs{})
	if err != nil {
		t.Fatal(err)
	}

	st, err := f.g.Stage(node)
	if err != nil {
		t.Fatal(err)
	}

	outs, err := s.outs(st, nil)
	if err != nil {
		t.Fatal(err)
	}

	return outs
}

// TestPostGates: a producer's post names the instances its outs disable:
// GATE when SRC.flag is true (mrp writes CH/GATE/fork0/_disabled), none
// when it is false, and none for a null gate, which mrp reads as false
// (null_gate: every gated call runs). A gate split per fork of a map call
// the producer feeds names each fork it disables by its ordinal
// (split_gate: mrp disables INNER/WORK/fork1, ITEM/WORK/fork0 and fork2),
// and so it does of a map call the entry forks, in the plan's order of the
// entry's elements (split_gate_entry: mrp disables INNER/WORK/fork0 and
// fork1, KEYED/WORK/fork_b and fork_c).
func TestPostGates(t *testing.T) {
	cases := []struct {
		fixture, node, outs, want string
	}{
		{"chain_gate", "CH.SRC", `{"y":4,"flag":true}`, "CH.GATE@@"},
		{"chain_gate", "CH.SRC", `{"y":4,"flag":false}`, ""},
		{"null_gate", "NULL_GATE.FLAG", `{"on":true,"cfg":{"flag":false,"k":1}}`, "NULL_GATE.DIRECT@@ NULL_GATE.MAPPED.WORK@@ NULL_GATE.SUB.WORK@@"},
		{"null_gate", "NULL_GATE.FLAG", `{"on":null,"cfg":{"flag":true,"k":1}}`, "NULL_GATE.FIELD@@"},
		{
			"split_gate", "SPLIT_GATE.SRC",
			`{"xs":[1,2,3],"skips":[false,true,false],"entries":[{"x":1,"skip":true},{"x":2,"skip":false},{"x":3,"skip":true}]}`,
			"SPLIT_GATE.INNER.WORK@SPLIT_GATE.INNER@1 SPLIT_GATE.ITEM.WORK@SPLIT_GATE.ITEM@0 SPLIT_GATE.ITEM.WORK@SPLIT_GATE.ITEM@2",
		},
		{
			"split_gate_entry", "SPLIT_GATE_ENTRY.SRC",
			`{"skips":[true,true,false],"flags":{"a":false,"b":true,"c":true}}`,
			"SPLIT_GATE_ENTRY.INNER.WORK@SPLIT_GATE_ENTRY.INNER@0 SPLIT_GATE_ENTRY.INNER.WORK@SPLIT_GATE_ENTRY.INNER@1 " +
				"SPLIT_GATE_ENTRY.KEYED.WORK@SPLIT_GATE_ENTRY.KEYED@1 SPLIT_GATE_ENTRY.KEYED.WORK@SPLIT_GATE_ENTRY.KEYED@2",
		},
	}

	for _, tc := range cases {
		f := loadFixture(t, tc.fixture)

		if got := f.postFields(f.outs(tc.node, nil, tc.outs)).gates; got != tc.want {
			t.Errorf("%s outs %s: gates %q, want %q", tc.node, tc.outs, got, tc.want)
		}
	}
}

// readCounter counts the outs reads a resolver makes through it.
type readCounter struct {
	store

	reads int
}

func (c *readCounter) outs(st *syntax.CallGraphStage, f Fork) (core.LazyArgumentMap, error) {
	c.reads++

	return c.store.outs(st, f)
}

// TestPostGatesReadOnce: FLAGS's post in bench/forkdisabled gates GATED's
// USE at every skipped fork, and reads FLAGS.skips as often at 64 forks as
// at 4: once per output and upstream fork, not once per gated fork (#550).
func TestPostGatesReadOnce(t *testing.T) {
	reads := map[int]int{}

	for _, n := range []int{4, 64} {
		outs := forkDisabledFlags(t, n)

		s, err := outs.g.stage([]string{outs.dir}, nil, shim.Refs{})
		if err != nil {
			t.Fatal(err)
		}

		c := &readCounter{store: s}
		path := filepath.Join(t.TempDir(), "gates")

		if err := outs.g.writeGates(Instance{Node: "FORKDISABLED.FLAGS"}, c, path); err != nil {
			t.Fatal(err)
		}

		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}

		if got := len(strings.Fields(string(raw))); got != n/2 {
			t.Errorf("forks=%d: %d gates, want %d", n, got, n/2)
		}

		reads[n] = c.reads
	}

	if reads[4] != reads[64] {
		t.Errorf("FLAGS outs reads: %d at 4 forks, %d at 64, want equal", reads[4], reads[64])
	}
}

// BenchmarkPostGates posts FLAGS's outs in bench/forkdisabled at two
// widths, 4-fold apart: its partner elements and the gates of GATED's USE,
// whose disable binding splits FLAGS.skips. ns/fork stays flat with the
// width when the post decodes FLAGS.skips once, not once per fork (#550).
func BenchmarkPostGates(b *testing.B) {
	for _, n := range []int{2000, 8000} {
		b.Run("forks="+strconv.Itoa(n), func(b *testing.B) {
			outs := forkDisabledFlags(b, n)

			spec := postedSpec(b, outs.g, outs.dir, "forks")

			for b.Loop() {
				dir := b.TempDir()
				spec.Forks, spec.ForkValues, spec.Gates = filepath.Join(dir, "forks"), filepath.Join(dir, "fork_values"), filepath.Join(dir, "gates")
				spec.Fields, spec.Values = filepath.Join(dir, "outs"), filepath.Join(dir, "outs_values")
				spec.FieldRefs, spec.ForkRefs = filepath.Join(dir, "outs_refs"), filepath.Join(dir, "fork_refs")

				if err := outs.g.Post(spec); err != nil {
					b.Fatal(err)
				}
			}

			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*n), "ns/fork")
		})
	}
}

// flagsOuts is FLAGS's staged outs bundle and the graph it is posted in.
type flagsOuts struct {
	g   *Graph
	dir string
}

// forkDisabledFlags compiles bench/forkdisabled at width n and stages
// FLAGS's outs, every other fork skipped.
func forkDisabledFlags(tb testing.TB, n int) flagsOuts {
	tb.Helper()

	xs, skips := make([]int, n), make([]bool, n)
	for i := range n {
		xs[i], skips[i] = i, i%2 == 1
	}

	xsJSON, err := json.Marshal(xs)
	if err != nil {
		tb.Fatal(err)
	}

	skipsJSON, err := json.Marshal(skips)
	if err != nil {
		tb.Fatal(err)
	}

	src := fmt.Sprintf("@include \"common.mro\"\n\ncall FORKDISABLED(xs = %s, skips = %s,)\n", xsJSON, skipsJSON)

	g, err := Compile([]byte(src), "pipeline.mro", []string{filepath.Join("..", "..", "bench", "forkdisabled")})
	if err != nil {
		tb.Fatal(err)
	}

	raw := json.RawMessage(`{"skips":` + string(skipsJSON) + `}`)

	dir := filepath.Join(tb.TempDir(), "outs")
	if err := writeOuts(dir, raw, g, "FLAGS"); err != nil {
		tb.Fatal(err)
	}

	if err := writeInstance(dir, Instance{Node: "FORKDISABLED.FLAGS"}); err != nil {
		tb.Fatal(err)
	}

	return flagsOuts{g: g, dir: dir}
}
