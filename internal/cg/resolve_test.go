package cg

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/eunmann/mro2nf/internal/types"
	"github.com/google/go-cmp/cmp"
	"github.com/martian-lang/martian/martian/core"
	"github.com/martian-lang/martian/martian/syntax"
)

// Every expected value in this file is copied from a real mrp run of the
// named testdata fixture (mrp built at the Martian commit go.mod pins, run
// as the e2e harness runs it): the _args mrp wrote to the named fork's
// split/_args, from the _outs its parents wrote. A file path mrp wrote is
// shortened to its pipestance-relative form on both sides.

// fixture is one testdata pipeline's call graph and a task's staged items.
type fixture struct {
	t   *testing.T
	g   *Graph
	mro string
	dir string
	seq int
}

func loadFixture(t *testing.T, name string) *fixture {
	t.Helper()

	dir := filepath.Join("..", "..", "testdata", name)
	mro := filepath.Join(dir, "pipeline.mro")

	g, err := Load(mro, "", nil)
	if err != nil {
		t.Fatal(err)
	}

	return &fixture{t: t, g: g, mro: mro, dir: t.TempDir()}
}

// writeOuts publishes the pipeline outs from the staged items in and
// returns the outs JSON, compacted, or the error.
func (f *fixture) writeOuts(in ...string) (string, error) {
	f.t.Helper()

	outs := f.next("pipeline_outs.json")
	spec := OutsSpec{In: in, Work: f.next("work"), Manifest: f.next("publish.json"), OutsJSON: outs}

	if err := f.g.WriteOuts(f.t.Context(), spec); err != nil {
		return "", err
	}

	raw, err := os.ReadFile(outs)
	if err != nil {
		f.t.Fatal(err)
	}

	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		f.t.Fatal(err)
	}

	return compact.String(), nil
}

func (f *fixture) next(name string) string {
	f.seq++

	return filepath.Join(f.dir, strconv.Itoa(f.seq), name)
}

// part is root fqid's part at an array index or map key.
func (f *fixture) part(root string, ix syntax.CollectionIndex) Part {
	f.t.Helper()

	r, err := f.g.forkRoot(root)
	if err != nil {
		f.t.Fatal(err)
	}

	return Part{Root: r, Index: ix}
}

// outs stages the outs mrp's stage wrote at fork as a stamped outs bundle.
func (f *fixture) outs(node string, fork Fork, outs string) string {
	f.t.Helper()

	st, err := f.g.Stage(node)
	if err != nil {
		f.t.Fatal(err)
	}

	dir := f.next("outs")
	if err := writeOuts(dir, json.RawMessage(outs), f.g, st.Call().DecId); err != nil {
		f.t.Fatal(err)
	}

	// Resolution never reads an instance's fork counts.
	if err := writeInstance(dir, Instance{Node: node, Fork: fork, Lens: make([]int, len(fork))}); err != nil {
		f.t.Fatal(err)
	}

	return dir
}

// writeOuts writes v, callable's outs, as an outs bundle that carries its
// files, as a task posts them.
func writeOuts(dir string, v json.Marshaler, g *Graph, callable string) error {
	payload, params, err := g.bundlePayload(v, callable, types.RoleOut, nil)
	if err != nil {
		return err
	}

	_, err = shim.WriteItem(dir, payload, params, g.Table(), shim.Opts{}, shim.Staged{})

	return err
}

// args runs WriteArgs for node over the items, dirs it stages and value
// records, returning the instance and its compact _args ("" when
// disabled).
func (f *fixture) args(node string, items ...string) (Instance, string) {
	f.t.Helper()

	return f.writeArgs(withItems(ArgsSpec{Node: node}, items...))
}

// withItems is spec getting the items, dirs it stages and value records.
func withItems(spec ArgsSpec, items ...string) ArgsSpec {
	spec.In, spec.Values = staging(items)

	return spec
}

// writeArgs is args for the instance spec stages, its files linked below a
// new dir.
func (f *fixture) writeArgs(spec ArgsSpec) (Instance, string) {
	f.t.Helper()

	node, out := spec.Node, f.next("args")
	spec.Out = out

	inst, args, err := f.g.WriteArgs(spec)
	if err != nil {
		f.t.Fatal(err)
	}

	if inst.Disabled {
		if args != nil {
			f.t.Errorf("%s: a disabled instance got args", node)
		}

		return inst, ""
	}

	return inst, compactArgs(f.t, args)
}

// compactArgs is args as one compact JSON object, its keys sorted.
func compactArgs(t *testing.T, args core.LazyArgumentMap) string {
	t.Helper()

	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}

	return string(raw)
}

// post posts the instance the outs bundle outs stamps with its outs, as
// its last job hands them to its post, and returns its forks dir, beside
// which it writes its element value records, as a task's dir holds them
// (forkValues), and its outs field by field.
func (f *fixture) post(outs string) string {
	f.t.Helper()

	forks := f.next("forks")
	if err := f.g.Post(postedSpec(f.t, f.g, outs, forks)); err != nil {
		f.t.Fatal(err)
	}

	return forks
}

// postedSpec is the post of the instance the outs bundle outs stamps, its
// outs read from it, writing its elements into forks and all else beside.
func postedSpec(tb testing.TB, g *Graph, outs, forks string) PostSpec {
	tb.Helper()

	in, err := g.readInstance(outs)
	if err != nil {
		tb.Fatal(err)
	}

	raw, err := shim.Refs{}.ReadBundle(outs)
	if err != nil {
		tb.Fatal(err)
	}

	var m core.LazyArgumentMap
	if err := json.Unmarshal(raw, &m); err != nil {
		tb.Fatal(err)
	}

	beside := filepath.Dir(forks)

	return PostSpec{
		Instance: in, Outs: m, Forks: forks, ForkValues: forkValues(forks), ForkRefs: filepath.Join(beside, "fork_refs"),
		Fields: filepath.Join(beside, "outs"), Values: filepath.Join(beside, "outs_values"), FieldRefs: filepath.Join(beside, "outs_refs"),
	}
}

// forkValues is the value records file of the post whose forks dir is
// forks.
func forkValues(forks string) string { return filepath.Join(filepath.Dir(forks), "fork_values") }

// elementItems are the items a post or the entry wrote of root, a fork
// root or a partner dir (PartnerDir), into forks: its element dirs and its
// value records, by name.
func elementItems(t *testing.T, forks, root string) []string {
	t.Helper()

	items, err := filepath.Glob(filepath.Join(forks, root, "*"))
	if err != nil {
		t.Fatal(err)
	}

	if raw, err := os.ReadFile(forkValues(forks)); err == nil {
		for rec := range strings.FieldsSeq(string(raw)) {
			if path, _, _ := strings.Cut(rec, ":"); filepath.Dir(path) == root {
				items = append(items, rec)
			}
		}
	}

	slices.SortFunc(items, func(a, b string) int { return strings.Compare(itemName(a), itemName(b)) })

	return items
}

// itemName is the name of an element item: its dir's, or the dir's its
// value record stands in for.
func itemName(item string) string {
	path, _, _ := strings.Cut(item, ":")

	return filepath.Base(path)
}

// itemElement is the fork element of g item names: a dir, or a value
// record.
func itemElement(t *testing.T, g *Graph, item string) Element {
	t.Helper()

	if filepath.IsAbs(item) {
		el, err := g.ReadElement(item)
		if err != nil {
			t.Fatal(err)
		}

		return el
	}

	el, ok, err := g.ValueElement(item)
	if err != nil || !ok {
		t.Fatalf("value record %.60q is no fork element: %v", item, err)
	}

	return el
}

// elementValue is the value a fork or partner element item carries: its
// bundle's, or its value record's.
func (f *fixture) elementValue(item string) string {
	f.t.Helper()

	it := elementItem{dir: item}

	if !filepath.IsAbs(item) {
		_, rec, err := decodeValue(item)
		if err != nil {
			f.t.Fatal(err)
		}

		it = elementItem{value: rec.Value}
	}

	raw, _, err := it.read(shim.Refs{})
	if err != nil {
		f.t.Fatal(err)
	}

	return string(raw)
}

// staging splits the items a task gets into the dirs it stages, by their
// absolute paths, and the value records it gets as values.
func staging(items []string) ([]string, []string) {
	var dirs, values []string

	for _, it := range items {
		if filepath.IsAbs(it) {
			dirs = append(dirs, it)
		} else {
			values = append(values, it)
		}
	}

	return dirs, values
}

func wantArgs(t *testing.T, node, got, want string) {
	t.Helper()

	if d := cmp.Diff(want, got); d != "" {
		t.Errorf("%s _args differ from mrp's (-mrp +got):\n%s", node, d)
	}
}

// TestRuntimeForkFromElement: GEN's post writes one fork element per key of
// its outs map, and a DBL instance staging only its own element resolves
// `v = split GEN.m` to that element (map_fork_upstream, MFU/DBL/fork_beta).
func TestRuntimeForkFromElement(t *testing.T) {
	f := loadFixture(t, "map_fork_upstream")

	gen := f.outs("MFU.GEN", nil, `{"m":{"alpha":1,"beta":2,"gamma":3}}`)
	forks := f.post(gen)

	elems := elementItems(t, forks, "MFU.DBL")
	if len(elems) != 3 {
		t.Fatalf("GEN posted %d forks of DBL, mrp ran 3", len(elems))
	}

	inst, got := f.args("MFU.DBL", elems[1])
	if inst.Fork[0].Index.MapKey() != "beta" {
		t.Errorf("element 0001 is fork %s, mrp's second fork is beta", inst.Fork)
	}

	wantArgs(t, "MFU.DBL", got, `{"v":2}`)
}

// TestMergeProjection: SUMX binds MAKE.p.x outside MAKE's map call, a merge
// over MAKE's forks projected through the struct (struct_proj, PP/SUMX).
func TestMergeProjection(t *testing.T) {
	f := loadFixture(t, "struct_proj")

	makes := []string{`{"x":1,"y":10}`, `{"x":2,"y":20}`, `{"x":3,"y":30}`}

	in := make([]string, 0, len(makes))
	for i, p := range makes {
		in = append(in, f.outs("PP.MAKE", Fork{f.part("PP.MAKE", ArrayIndex(i))}, `{"p":`+p+`}`))
	}

	_, got := f.args("PP.SUMX", in...)
	wantArgs(t, "PP.SUMX", got, `{"xs":[1,2,3]}`)
}

// TestWholeCallRefs: USE binds whole refs to an array- and a map-forked
// call, a merge of each fork's outs struct (map_wholeref, MW/USE).
func TestWholeCallRefs(t *testing.T) {
	f := loadFixture(t, "map_wholeref")

	in := []string{
		f.outs("MW.MAKEB", Fork{f.part("MW.MAKEB", ArrayIndex(0))}, `{"a":1,"n":10,"report":"MW/MAKEB/fork0/files/report.txt"}`),
		f.outs("MW.MAKEB", Fork{f.part("MW.MAKEB", ArrayIndex(1))}, `{"a":2,"n":20,"report":"MW/MAKEB/fork1/files/report.txt"}`),
		f.outs("MW.MAKEM", Fork{f.part("MW.MAKEM", MapKey("p"))}, `{"a":3,"n":30,"report":"MW/MAKEM/fork_p/files/report.txt"}`),
		f.outs("MW.MAKEM", Fork{f.part("MW.MAKEM", MapKey("q"))}, `{"a":4,"n":40,"report":"MW/MAKEM/fork_q/files/report.txt"}`),
	}

	_, got := f.args("MW.USE", in...)
	wantArgs(t, "MW.USE", got, `{"made":[{"a":1,"n":10,"report":"MW/MAKEB/fork0/files/report.txt"},`+
		`{"a":2,"n":20,"report":"MW/MAKEB/fork1/files/report.txt"}],`+
		`"madem":{"p":{"a":3,"n":30,"report":"MW/MAKEM/fork_p/files/report.txt"},`+
		`"q":{"a":4,"n":40,"report":"MW/MAKEM/fork_q/files/report.txt"}}}`)
}

// TestRuntimeDisable: GATE is disabled by SRC.flag = true, so it runs no
// job; USE still binds SRC.y (chain_gate, CH/GATE/fork0/_disabled, CH/USE).
func TestRuntimeDisable(t *testing.T) {
	f := loadFixture(t, "chain_gate")

	src := f.outs("CH.SRC", nil, `{"flag":true,"y":5}`)

	gate, got := f.args("CH.GATE", src)
	if !gate.Disabled || got != "" {
		t.Errorf("GATE disabled=%v args=%q, mrp disabled it", gate.Disabled, got)
	}

	_, got = f.args("CH.USE", src)
	wantArgs(t, "CH.USE", got, `{"k":3,"y":5}`)
}

// TestNullGateEnables: a disable gate bound to an upstream output that is
// null at run time leaves the call enabled (null_gate, NULL_GATE/DIRECT and
// NULL_GATE/FIELD).
func TestNullGateEnables(t *testing.T) {
	f := loadFixture(t, "null_gate")

	flag := f.outs("NULL_GATE.FLAG", nil, `{"cfg":{"flag":null,"k":1},"on":null}`)

	for _, node := range []string{"NULL_GATE.DIRECT", "NULL_GATE.FIELD"} {
		inst, got := f.args(node, flag)
		if inst.Disabled {
			t.Errorf("%s disabled by a null gate; mrp runs it", node)
		}

		wantArgs(t, node, got, `{"x":3}`)
	}
}

// TestStaticDisable: GEN's disable gate is the constant true once the call
// graph folds the entry value, so it never runs, and USE's binding to its
// output is null (disabled_entry_skip, P/GEN/fork0/_disabled, P/USE).
func TestStaticDisable(t *testing.T) {
	f := loadFixture(t, "disabled_entry_skip")

	gen, _ := f.args("P.GEN")
	if !gen.Disabled {
		t.Error("GEN runs; mrp disabled it")
	}

	_, got := f.args("P.USE")
	wantArgs(t, "P.USE", got, `{"y":null}`)
}

// TestEmptyForksPrune: map calls over an upstream's empty and null array
// and map run no fork, and their merges are the typed empty
// (runtime_empty_forks, REF/fork0/_outs).
func TestEmptyForksPrune(t *testing.T) {
	f := loadFixture(t, "runtime_empty_forks")

	gen := f.outs("REF.GEN", nil, `{"arr_empty":[],"arr_null":null,"m_empty":{},"m_null":null}`)
	forks := f.post(gen)

	for _, root := range []string{"REF.SC_EA", "REF.SC_NA", "REF.SC_EM", "REF.SC_NM"} {
		if n := len(elementItems(t, forks, root)); n != 0 {
			t.Errorf("GEN posted %d forks of %s, mrp ran none", n, root)
		}
	}

	raw, err := f.writeOuts(gen)
	if err != nil {
		t.Fatal(err)
	}

	wantArgs(t, "REF outs", raw, `{"ea":[],"em":{},"na":[],"nm":{}}`)
}

// TestKeyOrder: every way a struct value is built reaches CONS in mrp's
// key order: a projected or whole value keeps its producer's order, a value
// FilterJson narrows is rebuilt in declared order, and an object built
// member by member is sorted (struct_key_order, KO/CONS).
func TestKeyOrder(t *testing.T) {
	f := loadFixture(t, "struct_key_order")

	in := []string{
		f.outs("KO.PROD", nil, `{"a":2,"bx":{"b":3,"q":{"a":2,"m":9,"z":1}},`+
			`"fb":{"q":{"z":1,"a":2},"f":"KO/PROD/fork0/files/f.txt","b":3},"fl":{"a":2,"z":1},"m":9,`+
			`"s":{"z":1,"a":2},"um":{"y":1,"b":{"d":1,"c":2}},"w":{"a":2,"m":9,"z":1},"z":1}`),
		f.outs("KO.PRODK", Fork{f.part("KO.PRODK", MapKey("k1"))}, `{"a":40,"m":400,"z":4}`),
		f.outs("KO.PRODK", Fork{f.part("KO.PRODK", MapKey("k2"))}, `{"a":30,"m":300,"z":3}`),
		f.outs("KO.PRODM", Fork{f.part("KO.PRODM", ArrayIndex(0))}, `{"a":10,"m":100,"z":1}`),
		f.outs("KO.PRODM", Fork{f.part("KO.PRODM", ArrayIndex(1))}, `{"a":20,"m":200,"z":2}`),
	}

	_, got := f.args("KO.CONS", in...)
	wantArgs(t, "KO.CONS", got, `{"arr":[{"b":1,"q":{"z":1,"a":2}}],"b1":{"b":3,"q":{"z":1,"a":2}},`+
		`"b2":{"b":3,"q":{"z":1,"a":2}},"b3":{"q":{"z":1,"a":2},"b":3},"b4":{"b":7,"q":{"a":6,"z":5}},`+
		`"m1":{"y":1,"b":{"d":1,"c":2}},"mk":{"k1":{"a":40,"z":4},"k2":{"a":30,"z":3}},`+
		`"mp":{"k1":{"z":1,"a":2},"k2":{"z":1,"a":2}},"ms":[{"a":10,"z":1},{"a":20,"z":2}],`+
		`"s1":{"z":1,"a":2},"s2":{"z":1,"a":2},"s3":{"a":2,"z":1},"s4":{"a":2,"z":1}}`)
}

// TestUnstagedUpstreamFails: an instance whose parent's outs were not
// staged fails loudly rather than resolving a partial _args.
func TestUnstagedUpstreamFails(t *testing.T) {
	f := loadFixture(t, "chain_gate")

	_, _, err := f.g.WriteArgs(ArgsSpec{Node: "CH.USE", Out: f.next("args")})
	if err == nil {
		t.Fatal("USE resolved with SRC unstaged")
	}

	if _, statErr := os.Lstat(filepath.Join(f.dir, strconv.Itoa(f.seq), "args")); statErr == nil {
		t.Error("a failed resolution linked args files")
	}
}
