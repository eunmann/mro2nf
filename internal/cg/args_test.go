package cg

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/google/go-cmp/cmp"
)

// TestArgsLinkOneCopy pins one copy of a broadcast file (#498): every FUSE
// fork of bench/forks reads MKPAY's payload, and an args bundle holds no
// file of its own but its records. Each fork reads the
// payload, with the origin the producer recorded, through a link to the
// leaf it staged, so the payload's link count stays the producer's.
func TestArgsLinkOneCopy(t *testing.T) {
	mro := filepath.Join("..", "..", "bench", "forks", "pipeline_w4.mro")

	g, err := Load(mro, "", nil)
	if err != nil {
		t.Fatal(err)
	}

	f := &fixture{t: t, g: g, mro: mro, dir: t.TempDir()}

	src := filepath.Join(t.TempDir(), "payload.txt")
	if err := os.WriteFile(src, []byte("MRE_BENCH_FORKS_PROBE\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	mkpay := f.next("outs")
	if err := writeOuts(mkpay, json.RawMessage(`{"f":`+strconv.Quote(src)+`}`), g, "MKPAY"); err != nil {
		t.Fatal(err)
	}

	if err := writeInstance(mkpay, Instance{Node: "FANOUT.MKPAY"}); err != nil {
		t.Fatal(err)
	}

	leaf := filepath.Join(mkpay, "f", "L0000", "payload.txt")
	links := linkCount(t, leaf)

	entry := f.next("entry")
	if err := g.PostEntry(entry); err != nil {
		t.Fatal(err)
	}

	elems := elementDirs(t, entry, "FANOUT.FUSE")
	if len(elems) != 4 {
		t.Fatalf("the entry posted %d forks of FUSE, want 4", len(elems))
	}

	for i, el := range elems {
		args := f.next("args")
		if _, err := g.WriteArgs(ArgsSpec{Node: "FANOUT.FUSE", In: []string{mkpay, el}, Out: args}); err != nil {
			t.Fatal(err)
		}

		if d := cmp.Diff([]string{"data.json", "instance.json"}, regularFiles(t, args)); d != "" {
			t.Errorf("fork %d args files (-want +got):\n%s", i, d)
		}

		wantPayload(t, i, args, leaf)
	}

	if got := linkCount(t, leaf); got != links {
		t.Errorf("payload has %d links after the forks' args, want the producer's %d", got, links)
	}
}

// wantPayload requires fork i's args bundle to give FUSE the payload, with
// the origin recorded beside the producer's leaf.
func wantPayload(t *testing.T, i int, args, leaf string) {
	t.Helper()

	raw, err := shim.ReadBundle(args)
	if err != nil {
		t.Fatal(err)
	}

	var a struct {
		Payload string `json:"payload"`
		V       int    `json:"v"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatal(err)
	}

	if b, err := os.ReadFile(a.Payload); err != nil || string(b) != "MRE_BENCH_FORKS_PROBE\n" || a.V != i+1 {
		t.Errorf("fork %d _args %s: payload reads %q (%v), want the producer's file and v = %d", i, raw, b, err, i+1)
	}

	want, _, err := shim.ReadOrigin(leaf)
	if err != nil {
		t.Fatal(err)
	}

	if got, ok, err := shim.ReadOrigin(a.Payload); err != nil || !ok || got != want {
		t.Errorf("fork %d payload origin %+v (%v, %v), want the producer's %+v", i, got, ok, err, want)
	}
}

func linkCount(t *testing.T, path string) uint64 {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("no link count on this platform")
	}

	return st.Nlink
}

// regularFiles are the names of the regular files below dir, its links not
// followed.
func regularFiles(t *testing.T, dir string) []string {
	t.Helper()

	var out []string

	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			out = append(out, d.Name())
		}

		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	return out
}
