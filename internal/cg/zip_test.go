package cg

import (
	"bytes"
	"errors"
	"maps"
	"path/filepath"
	"slices"
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

	elems := elementDirs(t, f.post(three, three), "ZIP_RUNTIME.SHORT")
	if len(elems) != 3 {
		t.Fatalf("THREE posted %d forks of SHORT, mrp ran 3", len(elems))
	}

	var log bytes.Buffer

	out := f.next("args")
	if _, err := f.g.WriteArgs(ArgsSpec{Node: "ZIP_RUNTIME.SHORT", In: []string{elems[2], two}, Out: out, Log: &log}); err != nil {
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

	if n := len(elementDirs(t, f.post(three, three), "ZIP_RUNTIME.ORDER")); n != 0 {
		t.Errorf("THREE posted %d forks of ORDER; mrp forks it over TWO.xs", n)
	}

	elems := elementDirs(t, f.post(two, two), "ZIP_RUNTIME.ORDER")
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

	if n := len(elementDirs(t, f.post(none, none), "ZIP_RUNTIME.NULL_MASTER")); n != 0 {
		t.Errorf("NONE posted %d forks of NULL_MASTER, mrp ran none", n)
	}

	elems := elementDirs(t, f.post(two, two), "ZIP_RUNTIME.NULL_OTHER")
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

	short := elementDirs(t, entry, "ZIP_LITERAL.SHORT")
	if len(short) != 3 {
		t.Fatalf("the entry posted %d forks of SHORT, mrp ran 3", len(short))
	}

	_, got := f.args("ZIP_LITERAL.SHORT", short[2], two)
	wantArgs(t, "ZIP_LITERAL.SHORT fork2", got, `{"a":null,"b":30}`)

	outer := elementDirs(t, entry, "ZIP_LITERAL.INNER_SHORT")
	inner := elementDirs(t, entry, "ZIP_LITERAL.INNER_SHORT.PAIR")

	if len(outer) != 3 || len(inner) != 5 {
		t.Fatalf("the entry posted %d forks of INNER_SHORT and %d of its PAIR; mrp ran 3 and 5", len(outer), len(inner))
	}

	_, got = f.args("ZIP_LITERAL.INNER_SHORT.PAIR", outer[2], filepath.Join(filepath.Dir(inner[0]), "0002.0000"), two)
	wantArgs(t, "ZIP_LITERAL.INNER_SHORT.PAIR fork2_fork0", got, `{"a":null,"b":40}`)
}

// partnerDirs are the partner element dirs of node's output over root
// that node's post wrote into forks, by name.
func partnerDirs(t *testing.T, forks, root, node, output string) map[string]string {
	t.Helper()

	out := map[string]string{}
	for _, d := range elementDirs(t, forks, PartnerDir(root, node, output)) {
		out[filepath.Base(d)] = d
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

	short := elementDirs(t, f.post(three, three), "ZIP_RUNTIME.SHORT")
	twos := partnerDirs(t, f.post(two, two), "ZIP_RUNTIME.SHORT", "ZIP_RUNTIME.TWO", "xs")

	if d := cmp.Diff([]string{"0", "1", AbsentName}, slices.Sorted(maps.Keys(twos))); d != "" {
		t.Errorf("TWO's partner elements of SHORT (-want +got):\n%s", d)
	}

	_, got := f.args("ZIP_RUNTIME.SHORT", short[1], twos["1"])
	wantArgs(t, "ZIP_RUNTIME.SHORT fork1", got, `{"a":2,"b":20}`)

	var log bytes.Buffer

	out := f.next("args")
	if _, err := f.g.WriteArgs(ArgsSpec{Node: "ZIP_RUNTIME.SHORT", In: []string{short[2], twos[AbsentName]}, Out: out, Log: &log}); err != nil {
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

	other := elementDirs(t, f.post(two, two), "ZIP_RUNTIME.NULL_OTHER")
	nones := partnerDirs(t, f.post(none, none), "ZIP_RUNTIME.NULL_OTHER", "ZIP_RUNTIME.NONE", "xs")

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
	short := elementDirs(t, f.post(three, three), "ZIP_RUNTIME.SHORT")

	_, err := f.g.WriteArgs(ArgsSpec{Node: "ZIP_RUNTIME.SHORT", In: []string{short[0]}, Out: f.next("args")})
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
	fours := partnerDirs(t, f.post(four, four), "ZIP_LITERAL.INNER_LONG", "ZIP_LITERAL.FOUR", "xs")

	outer := elementDirs(t, entry, "ZIP_LITERAL.INNER_LONG")
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

	byAX := elementDirs(t, f.post(ax, ax), "ZIP_RUNTIME_MAP.BY_AX")
	abcs := partnerDirs(t, f.post(abc, abc), "ZIP_RUNTIME_MAP.BY_AX", "ZIP_RUNTIME_MAP.ABC", "m")

	if want := "0000@" + IndexToken(MapKey("a")); filepath.Base(byAX[0]) != want {
		t.Errorf("BY_AX fork a is named %q, want %q", filepath.Base(byAX[0]), want)
	}

	_, got := f.args("ZIP_RUNTIME_MAP.BY_AX", byAX[0], abcs[IndexToken(MapKey("a"))])
	wantArgs(t, "ZIP_RUNTIME_MAP.BY_AX fork_a", got, `{"a":10,"b":1}`)

	if _, ok := abcs[IndexToken(MapKey("x"))]; ok {
		t.Fatal("ABC wrote an element at key x, which its m lacks")
	}

	_, got = f.args("ZIP_RUNTIME_MAP.BY_AX", byAX[1], abcs[AbsentName])
	wantArgs(t, "ZIP_RUNTIME_MAP.BY_AX fork_x", got, `{"a":20,"b":null}`)
}
