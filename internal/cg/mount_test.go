package cg

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

// mountMRO's entry file values are s3:// URIs in a struct, an array and a
// map, a directory URI with a trailing / and one in upper case, beside a
// head-node path, a path given under the mount, and a string input that
// spells a URI but names no file.
const mountMRO = `filetype txt;

struct Ref(
    path dir,
    txt  fa,
)

stage READ(
    in  Ref      ref,
    in  txt[]    fs,
    in  map<txt> m,
    in  path     d,
    in  string   note,
    in  txt      given,
    out int      n,
    src py       "stages/read",
)

pipeline MNT(
    in  Ref      ref,
    in  txt[]    fs,
    in  map<txt> m,
    in  path     d,
    in  string   note,
    in  txt      given,
    out int      n,
)
{
    call READ(
        ref   = self.ref,
        fs    = self.fs,
        m     = self.m,
        d     = self.d,
        note  = self.note,
        given = self.given,
    )

    return (
        n = READ.n,
    )
}

call MNT(
    ref   = {
        dir: "s3://refs/genome/",
        fa:  "S3://refs/genome/fa.txt",
    },
    fs    = [
        "s3://reads/a.txt",
        "/data/b.txt",
    ],
    m     = {
        "x": "s3://reads/x.txt",
    },
    d     = "s3://reads",
    note  = "s3://reads/a.txt",
    given = "/mnt/s3/reads/given.txt",
)
`

// TestPlanMountsS3EntryFiles: under a mount, every s3:// file value of the
// entry inputs is its path under the mount in the call every task compiles,
// a directory's trailing / kept, a string that is no file left as it is.
// A mounted value, a path given under the mount too, keeps its index among
// the run's entry file values, which a task recomputes from the call, and no
// instance stages it; the head-node path is staged by its own index.
func TestPlanMountsS3EntryFiles(t *testing.T) {
	p := planUnder(t, mountMRO, "/mnt/s3")

	want := []EntryLeaf{
		{Input: "ref", Value: "/mnt/s3/refs/genome/", Mounted: true},
		{Input: "ref", Value: "/mnt/s3/refs/genome/fa.txt", Mounted: true},
		{Input: "fs", Value: "/mnt/s3/reads/a.txt", Mounted: true},
		{Input: "fs", Value: "/data/b.txt"},
		{Input: "m", Value: "/mnt/s3/reads/x.txt", Mounted: true},
		{Input: "d", Value: "/mnt/s3/reads", Mounted: true},
		{Input: "given", Value: "/mnt/s3/reads/given.txt", Mounted: true},
	}
	if d := cmp.Diff(want, p.Leaves); d != "" {
		t.Errorf("entry leaves (-want +plan):\n%s", d)
	}

	if !strings.Contains(p.Call, `"s3://reads/a.txt"`) {
		t.Errorf("the string input note was rewritten:\n%s", p.Call)
	}

	if d := cmp.Diff([]PlanFiles{{Leaves: []int{3}}}, p.Nodes[0].Files); d != "" {
		t.Errorf("READ's staged entry files (-want +plan):\n%s", d)
	}

	path := filepath.Join(t.TempDir(), "pipeline.mro")
	if err := os.WriteFile(path, []byte(mountMRO), 0o644); err != nil {
		t.Fatal(err)
	}

	g, err := Load(path, p.Call, nil)
	if err != nil {
		t.Fatal(err)
	}

	task, err := g.EntryLeaves(g.Manifest())
	if err != nil {
		t.Fatal(err)
	}

	for i := range want {
		want[i].Mounted = false
	}

	if d := cmp.Diff(want, task); d != "" {
		t.Errorf("a task's entry leaves of the planned call (-plan +task):\n%s", d)
	}
}

// TestMountEntryValuesRefusesUnmountableURIs: an s3:// URI with no bucket,
// or a key Mountpoint for S3 cannot show, fails the plan, naming the input
// and not the value; with no mount the invocation is left as it is.
func TestMountEntryValuesRefusesUnmountableURIs(t *testing.T) {
	src := sourceOf(t, mountMRO)

	if inv, err := MountEntryValues(src, src.Invocation, nil, ""); err != nil || inv != src.Invocation {
		t.Errorf("no mount: %v, %v, want the invocation as it is", inv, err)
	}

	for _, uri := range []string{"s3:///k.txt", "s3://b/a//k.txt", "s3://b/./k.txt", "s3://b/../k.txt", "s3:/b/k.txt", "s3://b/k//"} {
		inv := *src.Invocation
		inv.Args = map[string]json.RawMessage{"given": json.RawMessage(`"` + uri + `"`)}

		_, err := MountEntryValues(src, &inv, nil, "/mnt/s3")
		if !errors.Is(err, ErrEntryFile) || !strings.Contains(err.Error(), "'given'") || strings.Contains(err.Error(), uri) {
			t.Errorf("given %q: %v, want ErrEntryFile naming the input, not the value", uri, err)
		}
	}
}

// TestVetMountedLeaf: a mounted value must name an object under a bucket
// the mount shows, failing on a bucket the run's instances do not mount, a
// key no object has, and the mount root itself.
func TestVetMountedLeaf(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "b", "ref"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(root, "b", "k.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	cases := map[string]string{
		filepath.Join(root, "b", "k.txt"):        "",
		filepath.Join(root, "b", "ref") + "/":    "",
		filepath.Join(root, "b", "none.txt"):     "names no object under the mounted bucket b",
		filepath.Join(root, "other", "k.txt"):    "bucket other is not mounted at " + filepath.Join(root, "other") + " on this run's instances; add it to the stack's mountBuckets",
		root:                                     "names the S3 mount root",
		filepath.Join(root, "b", "..", "b", "k"): "names no object",
	}

	for value, want := range cases {
		err := VetMountedLeaf(EntryLeaf{Input: "reads", Value: value, Mounted: true}, shim.Mount(root))

		switch {
		case want == "" && err != nil:
			t.Errorf("vet %s: %v", value, err)
		case want != "" && (!errors.Is(err, ErrEntryFile) || !strings.Contains(err.Error(), want)):
			t.Errorf("vet %s: %v, want ErrEntryFile saying %q", value, err, want)
		}
	}
}
