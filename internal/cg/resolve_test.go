package cg

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/eunmann/mro2nf/internal/types"
	"github.com/google/go-cmp/cmp"
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
	dir string
	seq int
}

func loadFixture(t *testing.T, name string) *fixture {
	t.Helper()

	dir := filepath.Join("..", "..", "testdata", name)

	g, err := Load(filepath.Join(dir, "pipeline.mro"), nil)
	if err != nil {
		t.Fatal(err)
	}

	return &fixture{t: t, g: g, dir: t.TempDir()}
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
	if err := writeValue(dir, json.RawMessage(outs), types.Manifest{}, st.Call().DecId, types.RoleOut); err != nil {
		f.t.Fatal(err)
	}

	// Resolution never reads an instance's fork counts.
	if err := writeInstance(dir, Instance{Node: node, Fork: fork, Lens: make([]int, len(fork))}); err != nil {
		f.t.Fatal(err)
	}

	return dir
}

// args runs WriteArgs for node over the staged items, returning the
// instance and its compact _args ("" when disabled).
func (f *fixture) args(node string, in ...string) (Instance, string) {
	f.t.Helper()

	out := f.next("args")

	inst, err := f.g.WriteArgs(ArgsSpec{Node: node, In: in, Out: out})
	if err != nil {
		f.t.Fatal(err)
	}

	if inst.Disabled {
		if shim.IsBundle(out) {
			f.t.Errorf("%s: a disabled instance got an args payload", node)
		}

		return inst, ""
	}

	raw, err := shim.ReadBundle(out)
	if err != nil {
		f.t.Fatal(err)
	}

	return inst, string(raw)
}

// post posts the instance whose args bundle is args with its outs, and
// returns its forks dir.
func (f *fixture) post(args, outs string) string {
	f.t.Helper()

	forks := f.next("forks")
	if err := f.g.Post(PostSpec{Args: args, Outs: outs, Forks: forks}); err != nil {
		f.t.Fatal(err)
	}

	return forks
}

func elementDirs(t *testing.T, forks, root string) []string {
	t.Helper()

	ms, err := filepath.Glob(filepath.Join(forks, root, "*"))
	if err != nil {
		t.Fatal(err)
	}

	return ms
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
	forks := f.post(gen, gen)

	elems := elementDirs(t, forks, "MFU.DBL")
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
// job, and its post writes mrp's disabled outs, every output null; USE
// still binds SRC.y (chain_gate, CH/GATE/fork0/_disabled and _outs,
// CH/USE).
func TestRuntimeDisable(t *testing.T) {
	f := loadFixture(t, "chain_gate")

	src := f.outs("CH.SRC", nil, `{"flag":true,"y":5}`)

	gate, got := f.args("CH.GATE", src)
	if !gate.Disabled || got != "" {
		t.Errorf("GATE disabled=%v args=%q, mrp disabled it", gate.Disabled, got)
	}

	args := filepath.Join(f.dir, strconv.Itoa(f.seq), "args")
	outs := f.next("outs")
	f.post(args, outs)

	raw, err := shim.ReadBundle(outs)
	if err != nil {
		t.Fatal(err)
	}

	wantArgs(t, "CH.GATE _outs", string(raw), `{"w":null}`)

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
	forks := f.post(gen, gen)

	for _, root := range []string{"REF.SC_EA", "REF.SC_NA", "REF.SC_EM", "REF.SC_NM"} {
		if n := len(elementDirs(t, forks, root)); n != 0 {
			t.Errorf("GEN posted %d forks of %s, mrp ran none", n, root)
		}
	}

	out := f.next("pipeline")
	if err := f.g.WriteOuts(OutsSpec{In: []string{gen}, Out: out}); err != nil {
		t.Fatal(err)
	}

	raw, err := shim.ReadBundle(out)
	if err != nil {
		t.Fatal(err)
	}

	wantArgs(t, "REF outs", string(raw), `{"ea":[],"em":{},"na":[],"nm":{}}`)
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

	_, err := f.g.WriteArgs(ArgsSpec{Node: "CH.USE", Out: f.next("args")})
	if err == nil {
		t.Fatal("USE resolved with SRC unstaged")
	}

	if _, statErr := os.Stat(filepath.Join(f.dir, strconv.Itoa(f.seq), "args", "data.json")); statErr == nil {
		t.Error("a failed resolution wrote an args payload")
	}
}
