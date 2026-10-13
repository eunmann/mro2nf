package cg

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/eunmann/mro2nf/internal/types"
	"github.com/google/go-cmp/cmp"
	"github.com/martian-lang/martian/martian/syntax"
)

// TestElementsKeepValues: a fork element a post carries as a value record
// reads, from the record, the very JSON a reader read from its bundle,
// for each output of ECHO TestPostFieldsKeepsValues carries as a record,
// taken as an element of the output's type; and an element whose JSON is
// MaxValueBytes is a record, one byte more a bundle beside its fork record.
func TestElementsKeepValues(t *testing.T) {
	f := loadFixture(t, "marker_strings")

	st, err := f.g.Stage("MARKERS.ECHO")
	if err != nil {
		t.Fatal(err)
	}

	params, err := f.g.Params(st.Call().DecId, types.RoleOut)
	if err != nil {
		t.Fatal(err)
	}

	for _, lit := range []int{MaxValueBytes - 2, MaxValueBytes - 1} {
		var outs map[string]json.RawMessage
		if err := json.Unmarshal(fmt.Appendf(nil, echoOuts, strconv.Quote(strings.Repeat("x", lit)), "/w/made.txt"), &outs); err != nil {
			t.Fatal(err)
		}

		for _, p := range params {
			if files, err := f.g.Table().HasFiles(p.Tname); err != nil || files {
				continue
			}

			param := &syntax.StructMember{Id: elementField, Tname: p.Tname}
			rec, dir := f.postElement(outs[p.Id], param)

			if valued := len(outs[p.Id]) <= MaxValueBytes; valued != (rec != "") {
				t.Errorf("a %d-byte %s element: record %t, want %t", len(outs[p.Id]), p.Id, rec != "", valued)
			}

			got := f.readPosted(rec, dir)
			if d := cmp.Diff(f.bundled(outs[p.Id], param), got); d != "" {
				t.Errorf("a %d-byte %s element (-bundle +posted):\n%s", len(outs[p.Id]), p.Id, d)
			}
		}
	}
}

// postElement posts raw as fork 0 of a root, of param's type, and returns
// its value record, or else the dir it wrote.
func (f *fixture) postElement(raw json.RawMessage, param *syntax.StructMember) (string, string) {
	f.t.Helper()

	var recs, refs []string

	w := forkWriter{r: &resolver{g: f.g, st: emptyStore{}}, dir: f.next("forks"), tbl: f.g.Table(), values: true, recs: &recs, refs: &refs}
	fork := elementJSON{Lens: []int{1}}

	if err := w.item("ROOT", "0000", raw, param, valueJSON{Fork: &fork}); err != nil {
		f.t.Fatal(err)
	}

	dir := filepath.Join(w.dir, "ROOT", "0000")

	switch {
	case len(recs) == 1 && !exists(dir):
		return recs[0], ""
	case len(recs) == 0 && itemKind(dir) == elementFile:
		return "", dir
	default:
		f.t.Fatalf("an element posted %d records and dir %s of record %q", len(recs), dir, itemKind(dir))

		return "", ""
	}
}

// readPosted is the value a reader reads of the element posted as rec, or
// else as dir.
func (f *fixture) readPosted(rec, dir string) string {
	f.t.Helper()

	it := elementItem{dir: dir}

	if rec != "" {
		if name, v, err := decodeValue(rec); err != nil || name != "ROOT/0000" || v.Fork == nil {
			f.t.Fatalf("value record %s of %+v: %v", name, v, err)
		} else {
			it = elementItem{value: v.Value}
		}
	}

	raw, held, err := it.read(shim.Refs{})
	if err != nil || !held {
		f.t.Fatalf("read posted element: %t, %v", held, err)
	}

	return string(raw)
}

// bundled is the value a reader read of raw, of param's type, from its
// element bundle.
func (f *fixture) bundled(raw json.RawMessage, param *syntax.StructMember) string {
	f.t.Helper()

	v, err := types.DecodeJSON(raw)
	if err != nil {
		f.t.Fatal(err)
	}

	dir := f.next("bundle")
	if err := shim.WriteBundle(dir, map[string]any{param.Id: v}, []*syntax.StructMember{param}, f.g.Table(), shim.Opts{}); err != nil {
		f.t.Fatal(err)
	}

	got, _, err := elementItem{dir: dir}.read(shim.Refs{})
	if err != nil {
		f.t.Fatal(err)
	}

	return string(got)
}

// TestDecodeValueRejectsMalformed: a value record that is not a name, a
// colon and base64 of JSON holding one item's record and, but for an
// absent partner record, a value, fails as errBadValue rather than reading
// as a missing item.
func TestDecodeValueRejectsMalformed(t *testing.T) {
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

	for _, c := range []struct {
		name, text string
		bad        bool
	}{
		{"no colon", "y" + b64(`{"fork":{"lens":[1]},"value":1}`), true},
		{"no name", ":" + b64(`{"fork":{"lens":[1]},"value":1}`), true},
		{"bad base64", "y:!!", true},
		{"bad json", "y:" + b64(`{"fork":`), true},
		{"no item", "y:" + b64(`{"value":1}`), true},
		{"two items", "y:" + b64(`{"instance":{"node":"N"},"fork":{"lens":[1]},"value":1}`), true},
		{"no value", "y:" + b64(`{"fork":{"lens":[1]}}`), true},
		{"partner element with no value", "y:" + b64(`{"partner":{"root":"R","node":"N","output":"o","index":0}}`), true},
		{"fork element", "y:" + b64(`{"fork":{"lens":[1]},"value":1}`), false},
		{"null output", "y:" + b64(`{"instance":{"node":"N"},"value":null}`), false},
		{"absent partner record", "y:" + b64(`{"partner":{"root":"R","node":"N","output":"o"}}`), false},
	} {
		if _, _, err := decodeValue(c.text); errors.Is(err, errBadValue) != c.bad || (!c.bad && err != nil) {
			t.Errorf("%s: decode %q: %v, want malformed %t", c.name, c.text, err, c.bad)
		}
	}
}

// TestPostNeedsForkValues: a post writes the elements it feeds into a
// Forks dir, a ForkValues file and a ForkRefs file, as an element it
// carries as a value, or a file an element names by reference, would
// otherwise reach no reader.
func TestPostNeedsForkValues(t *testing.T) {
	f := loadFixture(t, "map_fork_upstream")
	gen := f.outs("MFU.GEN", nil, `{"m":{"alpha":1}}`)

	for _, drop := range []func(*PostSpec){func(s *PostSpec) { s.Forks = "" }, func(s *PostSpec) { s.ForkValues = "" }, func(s *PostSpec) { s.ForkRefs = "" }} {
		spec := postedSpec(t, f.g, gen, f.next("forks"))
		drop(&spec)

		if err := f.g.Post(spec); !errors.Is(err, errPostDirs) {
			t.Errorf("post with forks %q, fork values %q and refs %q: %v, want %v", spec.Forks, spec.ForkValues, spec.ForkRefs, err, errPostDirs)
		}
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)

	return err == nil
}

// TestPostCarriesFileLessElements: a post writes each fork and partner
// element whose type holds no file leaf as a value record and no dir, the
// absent record of a partner's output as a record too, and each element
// that holds files as a dir alone: GEN's int elements of DBL
// (map_fork_upstream), TWO's partner elements of SHORT (zip_runtime), and
// GEN's file elements of PROC (map_file_split).
func TestPostCarriesFileLessElements(t *testing.T) {
	mfu := loadFixture(t, "map_fork_upstream")
	gen := mfu.outs("MFU.GEN", nil, `{"m":{"alpha":1,"beta":2,"gamma":3}}`)
	zr := loadFixture(t, "zip_runtime")
	two := zr.outs("ZIP_RUNTIME.TWO", nil, `{"xs":[10,20]}`)
	fbs := loadFixture(t, "map_file_split")
	files := stageFileOuts(t, fbs, 2)

	for _, c := range []struct {
		f           *fixture
		outs, root  string
		dirs, names []string
	}{
		{mfu, gen, "MFU.DBL", nil, []string{"0000@" + IndexToken(MapKey("alpha")), "0001@" + IndexToken(MapKey("beta")), "0002@" + IndexToken(MapKey("gamma"))}},
		{zr, two, PartnerDir("ZIP_RUNTIME.SHORT", "ZIP_RUNTIME.TWO", "xs"), nil, []string{"0", "1", AbsentName}},
		{fbs, files, "FBS.PROC", []string{"0000", "0001"}, nil},
	} {
		forks := c.f.post(c.outs)

		var dirs, names []string

		for _, it := range elementItems(t, forks, c.root) {
			if filepath.IsAbs(it) {
				dirs = append(dirs, itemName(it))
			} else {
				names = append(names, itemName(it))
			}
		}

		if d := cmp.Diff(c.dirs, dirs); d != "" {
			t.Errorf("%s element dirs (-want +got):\n%s", c.root, d)
		}

		if d := cmp.Diff(c.names, names); d != "" {
			t.Errorf("%s element value records (-want +got):\n%s", c.root, d)
		}
	}
}
