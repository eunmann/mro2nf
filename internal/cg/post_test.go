package cg

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/google/go-cmp/cmp"
)

// TestForkElementKeys: GEN's post writes one fork element per map key, the
// ordinals in mrp's fork order, each key, whatever its characters, round
// tripping to its instance. The keys, the order and every fork's _args are
// those of a real mrp run of map_fork_upstream with GEN's map replaced by
// these keys (DBL's forks in _finalstate order, each fork's split/_args).
func TestForkElementKeys(t *testing.T) {
	f := loadFixture(t, "map_fork_upstream")

	gen := f.outs("MFU.GEN", nil, `{"m":{"a\tb":1,"line\nbreak":2,"x,y":3,"q\"uote":4,"ünï😀":5,"'single'":6,"Z":7,"a/b":8}}`)
	forks := f.post(gen)

	mrp := []struct {
		key string
		v   int
	}{
		{"'single'", 6},
		{"Z", 7},
		{"a\tb", 1},
		{"a/b", 8},
		{"line\nbreak", 2},
		{"q\"uote", 4},
		{"x,y", 3},
		{"ünï😀", 5},
	}

	elems := elementItems(t, forks, "MFU.DBL")
	if len(elems) != len(mrp) {
		t.Fatalf("GEN posted %d forks of DBL, mrp ran %d", len(elems), len(mrp))
	}

	for i, want := range mrp {
		if name, want := itemName(elems[i]), ordinal(i, len(mrp))+"@"+IndexToken(MapKey(want.key)); name != want {
			t.Errorf("fork %d is named %q, want %q", i, name, want)
		}

		if el := itemElement(t, f.g, elems[i]); el.Fork[0].Index.MapKey() != want.key || el.Position != i {
			t.Errorf("fork %d is key %q at position %d, mrp's is %q", i, el.Fork[0].Index.MapKey(), el.Position, want.key)
		}

		inst, got := f.args("MFU.DBL", elems[i])
		if inst.Fork[0].Index.MapKey() != want.key {
			t.Errorf("fork %d: instance key %q, want %q", i, inst.Fork[0].Index.MapKey(), want.key)
		}

		wantArgs(t, "MFU.DBL "+strconv.Quote(want.key), got, `{"v":`+strconv.Itoa(want.v)+`}`)
	}
}

// TestForkElementLeaves: each fork element of a split over GEN's file
// array holds that element's file and no other, and a PROC instance
// staging only its element reads the file mrp gives that fork
// (map_file_split: mrp's FBS/PROC/fork<i> reads g<i>.txt, i+1 bytes).
func TestForkElementLeaves(t *testing.T) {
	const n = 3

	f := loadFixture(t, "map_file_split")

	gen := stageFileOuts(t, f, n)

	forks := f.post(gen)

	elems := elementItems(t, forks, "FBS.PROC")
	if len(elems) != n {
		t.Fatalf("GEN posted %d forks of PROC, mrp ran %d", len(elems), n)
	}

	for i, dir := range elems {
		want := "g" + strconv.Itoa(i) + ".txt"
		// .mre-origin is the shim's record of where the leaf came from (#431).
		if d := cmp.Diff([]string{".mre-origin", want}, leafNames(t, dir)); d != "" {
			t.Errorf("fork %d element leaves (-want +got):\n%s", i, d)
		}

		path := procInput(t, f, dir)
		if b, err := os.ReadFile(path); err != nil || filepath.Base(path) != want || len(b) != i+1 {
			t.Errorf("fork %d reads %s (%d bytes, %v), want %s of %d bytes", i, path, len(b), err, want, i+1)
		}
	}
}

// TestForkElementLeaves2D (#553): an element of a txt[][] is a txt[], so
// each fork element of a split over MAKE.rows holds the files of its row,
// one of a split over the map<txt[]> MAKE.byk those of its key, and each
// partner element of MAKE.cols, which PAIR zips, those of its column, each
// staged, not left as a path into MAKE's task.
func TestForkElementLeaves2D(t *testing.T) {
	f := loadFixture(t, "map_file_2d")

	src := t.TempDir()
	file := func(name string) string {
		p := filepath.Join(src, name)
		if err := os.WriteFile(p, []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}

		return p
	}

	raw, err := json.Marshal(map[string]any{
		"rows": [][]string{{file("r0_0.txt"), file("r0_1.txt")}, {file("r1_0.txt")}},
		"cols": [][]string{{file("c0_0.txt"), file("c0_1.txt")}},
		"byk":  map[string][]string{"k0": {file("k0_0.txt"), file("k0_1.txt")}, "k1": {file("k1_0.txt")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	mk := f.outs("MAP_FILE_2D.MAKE", nil, string(raw))
	forks := f.post(mk)

	for root, want := range map[string][][]string{
		"MAP_FILE_2D.ROWS": {{"r0_0.txt", "r0_1.txt"}, {"r1_0.txt"}},
		"MAP_FILE_2D.BYK":  {{"k0_0.txt", "k0_1.txt"}, {"k1_0.txt"}},
		"MAP_FILE_2D.PAIR": {{"r0_0.txt", "r0_1.txt"}, {"r1_0.txt"}},
	} {
		elems := elementItems(t, forks, root)
		if len(elems) != len(want) {
			t.Fatalf("MAKE posted %d forks of %s, mrp ran %d", len(elems), root, len(want))
		}

		for i, dir := range elems {
			if d := cmp.Diff(want[i], stagedLeaves(t, dir)); d != "" {
				t.Errorf("%s fork %d element leaves (-want +got):\n%s", root, i, d)
			}
		}
	}

	cols := partnerItems(t, forks, "MAP_FILE_2D.PAIR", "MAP_FILE_2D.MAKE", "cols")
	if d := cmp.Diff([]string{"c0_0.txt", "c0_1.txt"}, stagedLeaves(t, cols["0"])); d != "" {
		t.Errorf("MAKE.cols partner element 0 leaves (-want +got):\n%s", d)
	}
}

// stagedLeaves are the names of the files staged in the bundle at dir, sorted.
func stagedLeaves(t *testing.T, dir string) []string {
	t.Helper()

	got := slices.DeleteFunc(leafNames(t, dir), func(n string) bool { return n == ".mre-origin" })
	slices.Sort(got)

	return got
}

// stageFileOuts stages FBS.GEN's outs as mrp's GEN writes them: n files,
// g<i>.txt of i+1 bytes.
func stageFileOuts(t *testing.T, f *fixture, n int) string {
	t.Helper()

	src := t.TempDir()
	files := make([]string, 0, n)

	for i := range n {
		p := filepath.Join(src, "g"+strconv.Itoa(i)+".txt")
		if err := os.WriteFile(p, bytes.Repeat([]byte("x"), i+1), 0o644); err != nil {
			t.Fatal(err)
		}

		files = append(files, p)
	}

	raw, err := json.Marshal(map[string][]string{"files": files})
	if err != nil {
		t.Fatal(err)
	}

	gen := f.next("outs")
	if err := writeOuts(gen, json.RawMessage(raw), f.g, "GEN"); err != nil {
		t.Fatal(err)
	}

	if err := writeInstance(gen, Instance{Node: "FBS.GEN"}); err != nil {
		t.Fatal(err)
	}

	return gen
}

// procInput is the file a PROC instance staging only the fork element at
// dir is given.
func procInput(t *testing.T, f *fixture, dir string) string {
	t.Helper()

	_, args, err := f.g.WriteArgs(ArgsSpec{Node: "FBS.PROC", In: []string{dir}, Out: f.next("args")})
	if err != nil {
		t.Fatal(err)
	}

	var path string
	if err := json.Unmarshal(args["f"], &path); err != nil {
		t.Fatal(err)
	}

	return path
}

// leafNames lists the files staged below a bundle's files dir.
func leafNames(t *testing.T, dir string) []string {
	t.Helper()

	var out []string

	err := filepath.WalkDir(filepath.Join(dir, "f"), func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			out = append(out, d.Name())
		}

		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	return out
}

// TestEntryForks: the entry posts the forks the call graph fixes, a map
// call over a literal array, and an instance staging one resolves its
// split from the graph (struct_proj: mrp's PP/MAKE/fork1 gets n = 2).
func TestEntryForks(t *testing.T) {
	f := loadFixture(t, "struct_proj")

	forks := f.next("forks")
	if err := f.g.PostEntry(forks); err != nil {
		t.Fatal(err)
	}

	elems := elementItems(t, forks, "PP.MAKE")
	if len(elems) != 3 {
		t.Fatalf("entry posted %d forks of MAKE, mrp ran 3", len(elems))
	}

	if shim.IsBundle(elems[1]) {
		t.Error("an entry fork element carries a value")
	}

	_, got := f.args("PP.MAKE", elems[1])
	wantArgs(t, "PP.MAKE fork1", got, `{"n":2}`)
}

// TestNestedEntryForkNames: a fork nested in an outer map call the entry
// also fixes is named by the outer fork's ordinal, then its own
// (fork_indep_call: SUB maps over [[1,2],[3]] inside OUTER, so mrp runs
// SUB's forks 0 and 1 under OUTER's fork 0 and fork 0 under OUTER's 1).
func TestNestedEntryForkNames(t *testing.T) {
	f := loadFixture(t, "fork_indep_call")

	forks := f.next("forks")
	if err := f.g.PostEntry(forks); err != nil {
		t.Fatal(err)
	}

	dirs := elementItems(t, forks, "TOP.OUTER.SUB")

	names := make([]string, 0, len(dirs))
	for _, d := range dirs {
		names = append(names, filepath.Base(d))
	}

	if d := cmp.Diff([]string{"0000.0000", "0000.0001", "0001.0000"}, names); d != "" {
		t.Errorf("SUB fork names (-want +got):\n%s", d)
	}
}

// TestForkRecordRoundTrip: a fork record keeps every key byte, so no key
// needs escaping on the way to the instance.
func TestForkRecordRoundTrip(t *testing.T) {
	f := loadFixture(t, "map_fork_upstream")

	for _, key := range []string{"a\tb", "line\nbreak", "x,y", `q"uote`, `back\slash`, "ünï😀", "", "\x00nul"} {
		dir := f.next("fork")

		want := Element{Fork: Fork{f.part("MFU.DBL", MapKey(key))}, Lens: []int{4}, Position: 3}
		if err := writeRecord(filepath.Join(dir, elementFile), want.encode()); err != nil {
			t.Fatal(err)
		}

		got, err := f.g.ReadElement(dir)
		if err != nil {
			t.Fatal(err)
		}

		if got.Fork.String() != want.Fork.String() || got.Position != want.Position || got.Fork[0].Index.MapKey() != key ||
			!slices.Equal(got.Lens, want.Lens) {
			t.Errorf("key %q round-trips to %s of %v at %d", key, got.Fork, got.Lens, got.Position)
		}
	}
}

// TestPostCountsForksFromTheSource: a post writes the forks of each root
// its stage's outs size, counted from the root's compiled source as mrp
// counts them (#595). MAKE sizes B, which maps over A's outs, from its own
// xs and m, and writes them as records only, since A's outs hold their
// elements (map_chain_runtime). A gate sizes a split of a gated literal
// under an outer map call, two forks per outer fork with the gate off and
// none with it on (rt_gate_split_nested's O_LIT).
func TestPostCountsForksFromTheSource(t *testing.T) {
	chain := loadFixture(t, "map_chain_runtime")
	forks := chain.post(chain.outs("MCR.MAKE", nil, `{"xs":[10,20,30],"m":{"b":2,"a":1}}`))

	for root, want := range map[string]int{"MCR.A": 3, "MCR.B": 3, "MCR.MA": 2, "MCR.MB": 2} {
		if got := len(elementItems(t, forks, root)); got != want {
			t.Errorf("MAKE's post wrote %d forks of %s, want %d", got, root, want)
		}
	}

	for _, item := range elementItems(t, forks, "MCR.B") {
		if !filepath.IsAbs(item) || shim.IsBundle(item) {
			t.Errorf("B's fork %s carries a value; A's outs hold its elements", item)
		}
	}

	nested := loadFixture(t, "rt_gate_split_nested")

	for off, want := range map[string]int{"false": 4, "true": 0} {
		forks := nested.post(nested.outs("TOP.G_ON", nil, `{"off":`+off+`}`))
		if got := len(elementItems(t, forks, "TOP.O_LIT.USE")); got != want {
			t.Errorf("gate off=%s: G_ON's post wrote %d forks of TOP.O_LIT.USE, want %d", off, got, want)
		}
	}
}

// TestPostFailsUnderAnOuterForkItDoesNotHold (#602): SRC feeds USE, whose
// forks vary with OUTER, a map call SRC is not forked on. Each instance of
// SRC holds only its own outs, so its post fails at the first fork of OUTER
// whose element another instance's outs hold, as mrp fails resolving USE's
// forks, rather than write USE's forks under every fork of OUTER from every
// instance of SRC.
func TestPostFailsUnderAnOuterForkItDoesNotHold(t *testing.T) {
	f := loadFixture(t, "post_outer_fork")

	for i := range 2 {
		outs := f.outs("TOP.INNER.SRC", Fork{f.part("TOP.INNER", ArrayIndex(i))}, `{"xs":[1,2,3]}`)

		err := f.g.Post(postedSpec(t, f.g, outs, f.next("forks")))
		if !errors.Is(err, ErrNotStaged) {
			t.Errorf("post of SRC at INNER fork %d: %v, want %v", i, err, ErrNotStaged)
		}
	}
}
