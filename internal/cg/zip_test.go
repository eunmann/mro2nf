package cg

import (
	"bytes"
	"errors"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/google/go-cmp/cmp"
)

// The expected _args below are those mrp wrote for the zip_runtime,
// zip_runtime_map and zip_literal fixtures
// (ZIP_RUNTIME/<call>/forkN/split/_args and likewise), run as the e2e
// harness runs mrp.

// TestZipShortSourceReadsNull: SHORT forks over THREE.xs, its first split,
// and reads TWO.xs at the same index, null past its end.
func TestZipShortSourceReadsNull(t *testing.T) {
	f := loadFixture(t, "zip_runtime")

	three := f.outs("ZIP_RUNTIME.THREE", nil, `{"xs":[1,2,3]}`)
	two := f.outs("ZIP_RUNTIME.TWO", nil, `{"xs":[10,20]}`)

	elems := elementItems(t, f.post(three, three), "ZIP_RUNTIME.SHORT")
	if len(elems) != 3 {
		t.Fatalf("THREE posted %d forks of SHORT, mrp ran 3", len(elems))
	}

	var log bytes.Buffer

	out := f.next("args")
	if _, err := f.g.WriteArgs(withItems(ArgsSpec{Node: "ZIP_RUNTIME.SHORT", Out: out, Log: &log}, elems[2], two)); err != nil {
		t.Fatal(err)
	}

	raw, err := shim.ReadBundle(out)
	if err != nil {
		t.Fatal(err)
	}

	wantArgs(t, "ZIP_RUNTIME.SHORT fork2", string(raw), `{"a":3,"b":null}`)

	if !strings.Contains(log.String(), "parameter b") || !strings.Contains(log.String(), "array index 2 >= 2") {
		t.Errorf("log %q does not report b's missing element, as mrp logs it", log.String())
	}
}

// TestZipForksFromFirstSplit: ORDER binds b = split TWO.xs before a = split
// THREE.xs, so TWO's post, not THREE's, writes its two forks.
func TestZipForksFromFirstSplit(t *testing.T) {
	f := loadFixture(t, "zip_runtime")

	three := f.outs("ZIP_RUNTIME.THREE", nil, `{"xs":[1,2,3]}`)
	two := f.outs("ZIP_RUNTIME.TWO", nil, `{"xs":[10,20]}`)

	if n := len(elementItems(t, f.post(three, three), "ZIP_RUNTIME.ORDER")); n != 0 {
		t.Errorf("THREE posted %d forks of ORDER; mrp forks it over TWO.xs", n)
	}

	elems := elementItems(t, f.post(two, two), "ZIP_RUNTIME.ORDER")
	if len(elems) != 2 {
		t.Fatalf("TWO posted %d forks of ORDER, mrp ran 2", len(elems))
	}

	_, got := f.args("ZIP_RUNTIME.ORDER", elems[1], three)
	wantArgs(t, "ZIP_RUNTIME.ORDER fork1", got, `{"a":2,"b":20}`)
}

// TestZipNullSource: a null master source forks NULL_MASTER zero times, and
// a null other source reads null at every fork of NULL_OTHER.
func TestZipNullSource(t *testing.T) {
	f := loadFixture(t, "zip_runtime")

	none := f.outs("ZIP_RUNTIME.NONE", nil, `{"xs":null}`)
	two := f.outs("ZIP_RUNTIME.TWO", nil, `{"xs":[10,20]}`)

	if n := len(elementItems(t, f.post(none, none), "ZIP_RUNTIME.NULL_MASTER")); n != 0 {
		t.Errorf("NONE posted %d forks of NULL_MASTER, mrp ran none", n)
	}

	elems := elementItems(t, f.post(two, two), "ZIP_RUNTIME.NULL_OTHER")
	if len(elems) != 2 {
		t.Fatalf("TWO posted %d forks of NULL_OTHER, mrp ran 2", len(elems))
	}

	_, got := f.args("ZIP_RUNTIME.NULL_OTHER", elems[0], none)
	wantArgs(t, "ZIP_RUNTIME.NULL_OTHER fork0", got, `{"a":10,"b":null}`)
}

// TestZipLiteralMaster: a split over a literal fixes the forks of SHORT and
// of INNER_SHORT, and TWO.xs reads null past its end, also inside the
// sub-pipeline, whose map call splits the literal's element again.
func TestZipLiteralMaster(t *testing.T) {
	f := loadFixture(t, "zip_literal")

	entry := f.next("entry")
	if err := f.g.PostEntry(entry); err != nil {
		t.Fatal(err)
	}

	two := f.outs("ZIP_LITERAL.TWO", nil, `{"xs":[1,2]}`)

	short := elementItems(t, entry, "ZIP_LITERAL.SHORT")
	if len(short) != 3 {
		t.Fatalf("the entry posted %d forks of SHORT, mrp ran 3", len(short))
	}

	_, got := f.args("ZIP_LITERAL.SHORT", short[2], two)
	wantArgs(t, "ZIP_LITERAL.SHORT fork2", got, `{"a":null,"b":30}`)

	outer := elementItems(t, entry, "ZIP_LITERAL.INNER_SHORT")
	inner := elementItems(t, entry, "ZIP_LITERAL.INNER_SHORT.PAIR")

	if len(outer) != 3 || len(inner) != 5 {
		t.Fatalf("the entry posted %d forks of INNER_SHORT and %d of its PAIR; mrp ran 3 and 5", len(outer), len(inner))
	}

	_, got = f.args("ZIP_LITERAL.INNER_SHORT.PAIR", outer[2], filepath.Join(filepath.Dir(inner[0]), "0002.0000"), two)
	wantArgs(t, "ZIP_LITERAL.INNER_SHORT.PAIR fork2_fork0", got, `{"a":null,"b":40}`)
}

// partnerItems are the partner element items of node's output over root
// that node's post wrote into forks, by name.
func partnerItems(t *testing.T, forks, root, node, output string) map[string]string {
	t.Helper()

	out := map[string]string{}
	for _, it := range elementItems(t, forks, PartnerDir(root, node, output)) {
		out[itemName(it)] = it
	}

	return out
}

// TestZipPartnerElements: a zipped split over another stage's output is
// served by that stage's partner element at the fork's index, so an
// instance stages one element of it: TWO's element at SHORT's index 1, and
// past TWO's end its absent record, which reads null and logs the missing
// element as mrp does. A null source's absent record reads null.
func TestZipPartnerElements(t *testing.T) {
	f := loadFixture(t, "zip_runtime")

	three := f.outs("ZIP_RUNTIME.THREE", nil, `{"xs":[1,2,3]}`)
	two := f.outs("ZIP_RUNTIME.TWO", nil, `{"xs":[10,20]}`)
	none := f.outs("ZIP_RUNTIME.NONE", nil, `{"xs":null}`)

	short := elementItems(t, f.post(three, three), "ZIP_RUNTIME.SHORT")
	twos := partnerItems(t, f.post(two, two), "ZIP_RUNTIME.SHORT", "ZIP_RUNTIME.TWO", "xs")

	if d := cmp.Diff([]string{"0", "1", AbsentName}, slices.Sorted(maps.Keys(twos))); d != "" {
		t.Errorf("TWO's partner elements of SHORT (-want +got):\n%s", d)
	}

	_, got := f.args("ZIP_RUNTIME.SHORT", short[1], twos["1"])
	wantArgs(t, "ZIP_RUNTIME.SHORT fork1", got, `{"a":2,"b":20}`)

	var log bytes.Buffer

	out := f.next("args")
	if _, err := f.g.WriteArgs(withItems(ArgsSpec{Node: "ZIP_RUNTIME.SHORT", Out: out, Log: &log}, short[2], twos[AbsentName])); err != nil {
		t.Fatal(err)
	}

	raw, err := shim.ReadBundle(out)
	if err != nil {
		t.Fatal(err)
	}

	wantArgs(t, "ZIP_RUNTIME.SHORT fork2", string(raw), `{"a":3,"b":null}`)

	if !strings.Contains(log.String(), "parameter b") {
		t.Errorf("log %q does not report b's missing element, as mrp logs it", log.String())
	}

	other := elementItems(t, f.post(two, two), "ZIP_RUNTIME.NULL_OTHER")
	nones := partnerItems(t, f.post(none, none), "ZIP_RUNTIME.NULL_OTHER", "ZIP_RUNTIME.NONE", "xs")

	if len(nones) != 1 {
		t.Fatalf("NONE wrote %d partner dirs for its null xs, want its absent record alone", len(nones))
	}

	_, got = f.args("ZIP_RUNTIME.NULL_OTHER", other[0], nones[AbsentName])
	wantArgs(t, "ZIP_RUNTIME.NULL_OTHER fork0", got, `{"a":10,"b":null}`)
}

// TestZipPartnerNotStagedFails: an instance that stages neither the
// partner element nor the other stage's outs fails, never reads null.
func TestZipPartnerNotStagedFails(t *testing.T) {
	f := loadFixture(t, "zip_runtime")

	three := f.outs("ZIP_RUNTIME.THREE", nil, `{"xs":[1,2,3]}`)
	short := elementItems(t, f.post(three, three), "ZIP_RUNTIME.SHORT")

	_, err := f.g.WriteArgs(withItems(ArgsSpec{Node: "ZIP_RUNTIME.SHORT", Out: f.next("args")}, short[0]))
	if !errors.Is(err, ErrNotStaged) {
		t.Errorf("WriteArgs = %v, want %v", err, ErrNotStaged)
	}
}

// TestZipPartnerUnderLiteralMaster: a literal fixes INNER_LONG's forks, so
// FOUR.xs is a partner there, also inside the sub-pipeline's map call (mrp
// INNER_LONG/PAIR/fork1: a = 2, b = 30).
func TestZipPartnerUnderLiteralMaster(t *testing.T) {
	f := loadFixture(t, "zip_literal")

	entry := f.next("entry")
	if err := f.g.PostEntry(entry); err != nil {
		t.Fatal(err)
	}

	four := f.outs("ZIP_LITERAL.FOUR", nil, `{"xs":[1,2,3,4]}`)
	fours := partnerItems(t, f.post(four, four), "ZIP_LITERAL.INNER_LONG", "ZIP_LITERAL.FOUR", "xs")

	outer := elementItems(t, entry, "ZIP_LITERAL.INNER_LONG")
	inner := filepath.Join(entry, "ZIP_LITERAL.INNER_LONG.PAIR", "0001.0000")

	_, got := f.args("ZIP_LITERAL.INNER_LONG.PAIR", outer[1], inner, fours["1"])
	wantArgs(t, "ZIP_LITERAL.INNER_LONG.PAIR fork1", got, `{"a":2,"b":30}`)
}

// TestZipPartnerMapKeys: zipped maps need not share keys at run time. An
// instance stages the other map's element by its key's token, and a key
// that map lacks reads null (mrp ZIP_RUNTIME_MAP: BY_AX fork_a a = 10,
// b = 1; fork_x a = 20, b = null). The master's fork element names carry
// the same token, which the driver joins them by.
func TestZipPartnerMapKeys(t *testing.T) {
	f := loadFixture(t, "zip_runtime_map")

	abc := f.outs("ZIP_RUNTIME_MAP.ABC", nil, `{"m":{"a":1,"b/c":2,"d e":3}}`)
	ax := f.outs("ZIP_RUNTIME_MAP.AX", nil, `{"m":{"a":10,"x":20}}`)

	byAX := elementItems(t, f.post(ax, ax), "ZIP_RUNTIME_MAP.BY_AX")
	abcs := partnerItems(t, f.post(abc, abc), "ZIP_RUNTIME_MAP.BY_AX", "ZIP_RUNTIME_MAP.ABC", "m")

	if want := "0000@" + IndexToken(MapKey("a")); itemName(byAX[0]) != want {
		t.Errorf("BY_AX fork a is named %q, want %q", itemName(byAX[0]), want)
	}

	_, got := f.args("ZIP_RUNTIME_MAP.BY_AX", byAX[0], abcs[IndexToken(MapKey("a"))])
	wantArgs(t, "ZIP_RUNTIME_MAP.BY_AX fork_a", got, `{"a":10,"b":1}`)

	if _, ok := abcs[IndexToken(MapKey("x"))]; ok {
		t.Fatal("ABC wrote an element at key x, which its m lacks")
	}

	_, got = f.args("ZIP_RUNTIME_MAP.BY_AX", byAX[1], abcs[AbsentName])
	wantArgs(t, "ZIP_RUNTIME_MAP.BY_AX fork_x", got, `{"a":20,"b":null}`)
}

// TestZipGatedPartner: a split over an output of a stage a runtime gate
// disables, `split MKFILES.fs unless GATE.off`, is a partner too (#494): a
// fork reads the gate, and MKFILES's element at its own index where the gate
// holds MKFILES enabled, never MKFILES's whole output; where the gate
// disables it, the fork reads null, staging no element.
func TestZipGatedPartner(t *testing.T) {
	mro := filepath.Join("..", "..", "bench", "zipgated", "pipeline_w4.mro")

	g, err := Load(mro, "", nil)
	if err != nil {
		t.Fatal(err)
	}

	topo, err := g.Topology()
	if err != nil {
		t.Fatal(err)
	}

	for _, n := range topo.Nodes {
		if n.Fqid != "ZIPGATED.USE" {
			continue
		}

		if d := cmp.Diff([]Zip{{Root: "ZIPGATED.USE", Node: "ZIPGATED.MKFILES", Output: "fs"}}, n.Zips); d != "" {
			t.Errorf("USE's zips (-want +got):\n%s", d)
		}

		if d := cmp.Diff([]Read{{Node: "ZIPGATED.GATE", Fields: []string{"off"}}}, n.Reads); d != "" {
			t.Errorf("USE's reads (-want +got):\n%s", d)
		}
	}

	f := &fixture{t: t, g: g, mro: mro, dir: t.TempDir()}

	vals := f.outs("ZIPGATED.MKVALS", nil, `{"vs":[0,1,2,3]}`)
	files := f.outs("ZIPGATED.MKFILES", nil, `{"fs":["/w/part0.txt","/w/part1.txt","/w/part2.txt","/w/part3.txt"]}`)
	use := elementItems(t, f.post(vals, vals), "ZIPGATED.USE")
	fs := partnerItems(t, f.post(files, files), "ZIPGATED.USE", "ZIPGATED.MKFILES", "fs")

	_, got := f.args("ZIPGATED.USE", use[1], fs["1"], f.outs("ZIPGATED.GATE", nil, `{"off":false}`))
	wantArgs(t, "ZIPGATED.USE fork1", got, `{"f":"/w/part1.txt","v":1}`)

	_, got = f.args("ZIPGATED.USE", use[1], f.outs("ZIPGATED.GATE", nil, `{"off":true}`))
	wantArgs(t, "ZIPGATED.USE fork1, MKFILES disabled", got, `{"f":null,"v":1}`)
}

// TestDisableGatePartner: a disable binding splitting another stage's
// output over a map call the entry forks, `disabled = split FLAGS.skips`,
// is served by FLAGS's partner element at the fork's index, so an enabled
// fork stages one element of FLAGS.skips, never the whole output, and
// resolves its disabled state from it as mrp does.
func TestDisableGatePartner(t *testing.T) {
	mro := filepath.Join("..", "..", "bench", "forkdisabled", "pipeline_w4.mro")

	g, err := Load(mro, "", nil)
	if err != nil {
		t.Fatal(err)
	}

	topo, err := g.Topology()
	if err != nil {
		t.Fatal(err)
	}

	for _, n := range topo.Nodes {
		if n.Fqid != "FORKDISABLED.GATED.USE" {
			continue
		}

		if d := cmp.Diff([]Zip{{Root: "FORKDISABLED.GATED", Node: "FORKDISABLED.FLAGS", Output: "skips"}}, n.Zips); d != "" {
			t.Errorf("USE's zips (-want +got):\n%s", d)
		}

		if d := cmp.Diff([]Read{{Node: "FORKDISABLED.GATED.MKFILE", Fields: []string{"f"}}}, n.Reads); d != "" {
			t.Errorf("USE's reads (-want +got):\n%s", d)
		}
	}

	f := &fixture{t: t, g: g, mro: mro, dir: t.TempDir()}

	flags := f.outs("FORKDISABLED.FLAGS", nil, `{"skips":[false,true,false,true]}`)
	skips := partnerItems(t, f.post(flags, flags), "FORKDISABLED.GATED", "FORKDISABLED.FLAGS", "skips")

	for i, want := range []string{`{"f":"/w/f0.txt"}`, ""} {
		got := f.entryForkArgs("FORKDISABLED.GATED.USE", "FORKDISABLED.GATED", i, skips[strconv.Itoa(i)])
		wantArgs(t, "FORKDISABLED.GATED.USE fork"+strconv.Itoa(i), got, want)
	}
}

// entryForkArgs writes the args of node's instance at fork i of root, an
// entry-fed root, getting items and MKFILE's outs at that fork, as mre
// stage's args step does; "" when the instance is disabled.
func (f *fixture) entryForkArgs(node, root string, i int, items ...string) string {
	f.t.Helper()

	entry := f.next("entry")
	if err := f.g.PostEntry(entry); err != nil {
		f.t.Fatal(err)
	}

	el := itemElement(f.t, f.g, elementItems(f.t, entry, root)[i])
	mk := f.outs("FORKDISABLED.GATED.MKFILE", el.Fork, `{"f":"/w/f`+strconv.Itoa(i)+`.txt"}`)
	out := f.next("args")

	inst, err := f.g.WriteArgs(withItems(ArgsSpec{Node: node, Forks: []Element{el}, Out: out}, append(items, mk)...))
	if err != nil {
		f.t.Fatal(err)
	}

	if inst.Disabled {
		return ""
	}

	raw, err := shim.ReadBundle(out)
	if err != nil {
		f.t.Fatal(err)
	}

	return string(raw)
}
