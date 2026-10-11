package cg

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/eunmann/mro2nf/internal/frontend"
	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/eunmann/mro2nf/internal/types"
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
	forks := f.post(gen, gen)

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

	elems := elementDirs(t, forks, "MFU.DBL")
	if len(elems) != len(mrp) {
		t.Fatalf("GEN posted %d forks of DBL, mrp ran %d", len(elems), len(mrp))
	}

	for i, want := range mrp {
		if name := filepath.Base(elems[i]); name != ordinal(i, len(mrp)) {
			t.Errorf("fork %d is named %q, want %q", i, name, ordinal(i, len(mrp)))
		}

		el, err := f.g.ReadElement(elems[i])
		if err != nil {
			t.Fatal(err)
		}

		if got := el.Fork[0].Index.MapKey(); got != want.key || el.Position != i {
			t.Errorf("fork %d is key %q at position %d, mrp's is %q", i, got, el.Position, want.key)
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
	man := fixtureManifest(t, "map_file_split")

	gen := stageFileOuts(t, f, man, n)

	forks := f.next("forks")
	if err := f.g.Post(PostSpec{Args: gen, Outs: gen, Manifest: man, Forks: forks}); err != nil {
		t.Fatal(err)
	}

	elems := elementDirs(t, forks, "FBS.PROC")
	if len(elems) != n {
		t.Fatalf("GEN posted %d forks of PROC, mrp ran %d", len(elems), n)
	}

	for i, dir := range elems {
		want := "g" + strconv.Itoa(i) + ".txt"
		// .mre-origin is the shim's record of where the leaf came from (#431).
		if d := cmp.Diff([]string{".mre-origin", want}, leafNames(t, dir)); d != "" {
			t.Errorf("fork %d element leaves (-want +got):\n%s", i, d)
		}

		path := procInput(t, f, man, dir)
		if b, err := os.ReadFile(path); err != nil || filepath.Base(path) != want || len(b) != i+1 {
			t.Errorf("fork %d reads %s (%d bytes, %v), want %s of %d bytes", i, path, len(b), err, want, i+1)
		}
	}
}

// stageFileOuts stages FBS.GEN's outs as mrp's GEN writes them: n files,
// g<i>.txt of i+1 bytes.
func stageFileOuts(t *testing.T, f *fixture, man types.Manifest, n int) string {
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
	if err := writeValue(gen, json.RawMessage(raw), man, "GEN", types.RoleOut); err != nil {
		t.Fatal(err)
	}

	if err := writeInstance(gen, Instance{Node: "FBS.GEN"}); err != nil {
		t.Fatal(err)
	}

	return gen
}

// procInput is the file a PROC instance staging only the fork element at
// dir is given.
func procInput(t *testing.T, f *fixture, man types.Manifest, dir string) string {
	t.Helper()

	args := f.next("args")
	if _, err := f.g.WriteArgs(ArgsSpec{Node: "FBS.PROC", In: []string{dir}, Manifest: man, Out: args}); err != nil {
		t.Fatal(err)
	}

	raw, err := shim.ReadBundle(args)
	if err != nil {
		t.Fatal(err)
	}

	var a struct {
		F string `json:"f"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatal(err)
	}

	return a.F
}

func fixtureManifest(t *testing.T, name string) types.Manifest {
	t.Helper()

	dir := filepath.Join("..", "..", "testdata", name)

	ast, err := frontend.Parse(filepath.Join(dir, "pipeline.mro"), []string{dir}, false)
	if err != nil {
		t.Fatal(err)
	}

	prog, err := frontend.Lower(ast, []string{dir})
	if err != nil {
		t.Fatal(err)
	}

	return types.BuildManifest(prog)
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

	elems := elementDirs(t, forks, "PP.MAKE")
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

	dirs := elementDirs(t, forks, "TOP.OUTER.SUB")

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

		want := Element{Fork: Fork{f.part("MFU.DBL", MapKey(key))}, Position: 3}
		if err := writeElement(dir, want); err != nil {
			t.Fatal(err)
		}

		got, err := f.g.ReadElement(dir)
		if err != nil {
			t.Fatal(err)
		}

		if got.Fork.String() != want.Fork.String() || got.Position != want.Position || got.Fork[0].Index.MapKey() != key {
			t.Errorf("key %q round-trips to %s at %d", key, got.Fork, got.Position)
		}
	}
}
