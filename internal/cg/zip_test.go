package cg

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/martian-lang/martian/martian/syntax"
)

// The expected _args below are those mrp wrote for the zip_runtime and
// zip_literal fixtures (ZIP_RUNTIME/<call>/forkN/split/_args and
// ZIP_LITERAL/...), run as the e2e harness runs mrp.

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

// TestZipStrictFails: under the error enforcement level a missing element
// fails the instance, as mrp's getBindings fails the fork.
func TestZipStrictFails(t *testing.T) {
	f := loadFixture(t, "zip_runtime")

	three := f.outs("ZIP_RUNTIME.THREE", nil, `{"xs":[1,2,3]}`)
	two := f.outs("ZIP_RUNTIME.TWO", nil, `{"xs":[10,20]}`)
	elems := elementDirs(t, f.post(three, three), "ZIP_RUNTIME.SHORT")

	level := syntax.GetEnforcementLevel()
	syntax.SetEnforcementLevel(syntax.EnforceError)

	t.Cleanup(func() { syntax.SetEnforcementLevel(level) })

	_, err := f.g.WriteArgs(ArgsSpec{Node: "ZIP_RUNTIME.SHORT", In: []string{elems[2], two}, Out: f.next("args")})
	if err == nil || !errors.Is(err, errNoElement) || !strings.Contains(err.Error(), "resolve input bindings") {
		t.Errorf("WriteArgs = %v, want the instance's unresolved input bindings, as mrp fails the fork", err)
	}
}
