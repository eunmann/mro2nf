package shim_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/google/go-cmp/cmp"
)

// refTask is a task that staged a producer's outs bundle as in/u1, linked
// the bundle's file and directory leaves below its args dir, and wrote a
// file of its own.
type refTask struct {
	dir, own, produced   string
	file, inner, ownLeaf string
	args                 map[string]any
	staged               map[string]any
}

// newRefTask stages the producer's bundle into a task below root as stage
// does: by a copy, as an object store stages it, or by a link.
func newRefTask(t *testing.T, root string, stage func(src, dst string) error) refTask {
	t.Helper()

	own := writeSelfNamed(t, filepath.Join(root, "producer", "main", "files", "y.txt"))
	d := filepath.Join(root, "producer", "main", "files", "d")
	writeSelfNamed(t, filepath.Join(d, "inner.txt"))

	r := refTask{dir: filepath.Join(root, "task"), own: own}
	r.produced = writeStageOuts(t, filepath.Join(root, "producer"), map[string]any{"f": own, "d": d}, fields(t, "f file", "d path"))

	if err := os.MkdirAll(filepath.Join(r.dir, "in"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := stage(r.produced, filepath.Join(r.dir, "in", "u1")); err != nil {
		t.Fatal(err)
	}

	t.Chdir(r.dir)

	r.staged = readPayload(t, filepath.Join("in", "u1"))
	r.args = linkLeaves(t, "args", map[string]any{"up": r.staged["f"], "dir": r.staged["d"]}, fields(t, "up file", "dir path"), shim.Roots{})
	r.ownLeaf = writeSelfNamed(t, filepath.Join(r.dir, "main", "files", "mine.txt"))
	r.file, _ = r.args["up"].(string)
	dir, _ := r.args["dir"].(string)
	r.inner = filepath.Join(dir, "inner.txt")

	return r
}

// TestWriteItemNamesStagedLeavesByReference (#562): a post names a file
// the task staged by reference to where its producer wrote it, not a copy,
// whether Nextflow staged the producer's bundle by a link or, as on an
// object store, by a copy: the task's own links name the item it lies in.
// One reference serves every position of one origin, the leaf passed
// through the args and the staged path alike. The task's own file, and a
// file inside a staged directory leaf, are copied as before, numbered
// without gaps; a reader resolves the reference among the leaves it staged
// by reference, the producer's file and origin, and fails without them.
func TestWriteItemNamesStagedLeavesByReference(t *testing.T) {
	for name, stage := range map[string]func(src, dst string) error{"copy": shim.CopyTree, "link": os.Symlink} {
		t.Run(name, func(t *testing.T) {
			r := newRefTask(t, t.TempDir(), stage)

			conf, err := shim.ConfineStage(r.dir, "main", true, shim.Roots{})
			if err != nil {
				t.Fatal(err)
			}

			staged, err := shim.NewStaged("in", "", "refs")
			if err != nil {
				t.Fatal(err)
			}

			params := fields(t, "up file", "chunk file", "own file", "inner file")
			payload := map[string]any{"up": r.file, "chunk": r.staged["f"], "own": r.ownLeaf, "inner": r.inner}

			refs, err := shim.WriteItem(filepath.Join("outs", "x"), payload, params, fileTable(t), shim.Opts{Confine: conf}, staged)
			if err != nil {
				t.Fatal(err)
			}

			if d := cmp.Diff([]shim.Ref{{Item: "in/u1", Ordinal: "f/L0000"}}, refs); d != "" {
				t.Errorf("references (-want +got):\n%s", d)
			}

			wantRefMarkers(t, filepath.Join("outs", "x"), r.own)
			wantReadByReference(t, r)
		})
	}
}

// wantRefMarkers requires the bundle at dir to name up and chunk by one
// reference to own's origin, and to hold copies of own and inner alone, in
// L0000 and L0001.
func wantRefMarkers(t *testing.T, dir, own string) {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(dir, "data.json"))
	if err != nil {
		t.Fatal(err)
	}

	var stored map[string]string
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}

	want := map[string]string{
		"up": "@mre:ref:" + own, "chunk": "@mre:ref:" + own,
		"own": "@mre:file:f/L0000/mine.txt", "inner": "@mre:file:f/L0001/inner.txt",
	}
	if d := cmp.Diff(want, stored); d != "" {
		t.Errorf("data.json (-want +got):\n%s", d)
	}
}

// wantReadByReference requires a reader that staged r's referenced leaf by
// reference, a copy of its ordinal dir at refs/1/f/L0000, to read the
// referenced positions as that leaf, of the producer's origin, and one that
// staged none to fail.
func wantReadByReference(t *testing.T, r refTask) {
	t.Helper()

	staged, _ := r.staged["f"].(string)
	if err := shim.CopyTree(filepath.Dir(staged), filepath.Join("refs", "1", "f", "L0000")); err != nil {
		t.Fatal(err)
	}

	refs, err := shim.ReadRefs("refs")
	if err != nil {
		t.Fatal(err)
	}

	raw, err := refs.ReadBundle(filepath.Join("outs", "x"))
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	leaf := filepath.Join(r.dir, "refs", "1", "f", "L0000", "y.txt")
	if got["up"] != leaf || got["chunk"] != leaf {
		t.Errorf("up %s, chunk %s, want the leaf staged by reference %s", got["up"], got["chunk"], leaf)
	}

	if o, ok, err := shim.ReadOrigin(leaf); err != nil || !ok || o != (shim.Origin{Path: r.own}) {
		t.Errorf("the leaf staged by reference has origin %+v (%t, %v), want its producer's %s", o, ok, err, r.own)
	}

	if b, err := os.ReadFile(got["own"]); err != nil || string(b) != r.ownLeaf {
		t.Errorf("own reads %q (%v), want the task's own file", b, err)
	}

	if _, err := shim.ReadBundle(filepath.Join("outs", "x")); err == nil || !strings.Contains(err.Error(), "not staged") {
		t.Errorf("a read with nothing staged by reference: %v, want it refused", err)
	}
}

// TestWriteItemCopiesWithNothingStaged: with no staged dir, WriteItem
// copies every leaf, as WriteBundle does, and returns no reference.
func TestWriteItemCopiesWithNothingStaged(t *testing.T) {
	r := newRefTask(t, t.TempDir(), os.Symlink)

	refs, err := shim.WriteItem(filepath.Join("outs", "x"), map[string]any{"up": r.file}, fields(t, "up file"), fileTable(t), shim.Opts{}, shim.Staged{})
	if err != nil || len(refs) != 0 {
		t.Fatalf("WriteItem: %v, %v; want no reference", refs, err)
	}

	if d := cmp.Diff(map[string]any{"up": filepath.Join(r.dir, "outs", "x", "f", "L0000", "y.txt")}, readPayload(t, filepath.Join("outs", "x"))); d != "" {
		t.Errorf("payload (-want +got):\n%s", d)
	}
}

// TestReadRefsRefusesAnOrdinalOfNoOneRecordedLeaf: a leaf staged by
// reference is one leaf beside its origin record; an ordinal dir with two,
// or with no record, fails the read.
func TestReadRefsRefusesAnOrdinalOfNoOneRecordedLeaf(t *testing.T) {
	for name, files := range map[string][]string{"two leaves": {".mre-origin", "a.txt", "b.txt"}, "no record": {"a.txt"}} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			ordinal := filepath.Join(dir, "1", "f", "L0000")

			for _, f := range files {
				writeSelfNamed(t, filepath.Join(ordinal, f))
			}

			if f := filepath.Join(ordinal, ".mre-origin"); fileExists(f) {
				if err := os.WriteFile(f, []byte("/w/a.txt"), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			if _, err := shim.ReadRefs(dir); err == nil {
				t.Error("ReadRefs succeeded")
			}
		})
	}
}

// TestRefMarkerIsReserved: a reference names an absolute origin, which
// CheckReserved accepts; any other string after the reference prefix is
// no marker mre writes.
func TestRefMarkerIsReserved(t *testing.T) {
	for s, ok := range map[string]bool{"@mre:ref:/w/a.txt": true, "@mre:ref:linked:/w/a.txt": true, "@mre:ref:a.txt": false, "@mre:ref:": false} {
		if err := shim.CheckReserved(s); (err == nil) != ok || (err != nil && !errors.Is(err, shim.ErrBadMarker)) {
			t.Errorf("CheckReserved(%q) = %v, want accepted %t", s, err, ok)
		}
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)

	return err == nil
}
