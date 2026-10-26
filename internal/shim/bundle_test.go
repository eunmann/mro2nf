package shim_test

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/eunmann/mro2nf/internal/types/typestest"
	"github.com/google/go-cmp/cmp"
	"github.com/martian-lang/martian/martian/syntax"
)

// writeBundle writes payload as a bundle at dir as a task that staged
// nothing writes it (shim.WriteItem), copying every file leaf.
func writeBundle(t *testing.T, dir string, payload map[string]any, params []*syntax.StructMember, opts shim.Opts) {
	t.Helper()

	if _, err := shim.WriteItem(dir, payload, params, fileTable(t), opts, shim.Staged{}); err != nil {
		t.Fatalf("WriteItem %s: %v", dir, err)
	}
}

// writeMarked writes payload as writeBundle does and returns the payload the
// bundle stores, each file leaf as its marker.
func writeMarked(t *testing.T, dir string, payload map[string]any, params []*syntax.StructMember) map[string]any {
	t.Helper()

	writeBundle(t, dir, payload, params, shim.Opts{})

	raw, err := os.ReadFile(filepath.Join(dir, "data.json"))
	if err != nil {
		t.Fatal(err)
	}

	var stored map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}

	return stored
}

// TestWriteItemKeepsBasename guards #284: a stage reads sample names and
// extensions from its inputs' basenames, so each leaf keeps the basename mrp
// would pass, under its own ordinal dir so leaves sharing a basename never
// collide. A second bundle written from the first's resolved paths keeps it too.
func TestWriteItemKeepsBasename(t *testing.T) {
	dir := t.TempDir()

	samples := []string{"sampleA", "sampleB"}
	srcs := make([]any, 0, len(samples))

	for i, sample := range samples {
		src := filepath.Join(dir, sample, "reads.fastq.gz")
		if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(src, []byte{byte('a' + i)}, 0o644); err != nil {
			t.Fatal(err)
		}

		srcs = append(srcs, src)
	}

	params := typestest.Fields(t, "fs fastq.gz[]")
	first := filepath.Join(dir, "first")
	writeBundle(t, first, map[string]any{"fs": srcs}, params, shim.Opts{})

	second := filepath.Join(dir, "second")
	marked := writeMarked(t, second, readPayload(t, first), params)

	want := map[string]any{"fs": []any{"@mre:file:f/L0000/reads.fastq.gz", "@mre:file:f/L0001/reads.fastq.gz"}}
	if diff := cmp.Diff(want, marked); diff != "" {
		t.Errorf("markers (-want +got):\n%s", diff)
	}

	for i, rel := range []string{"f/L0000/reads.fastq.gz", "f/L0001/reads.fastq.gz"} {
		got, err := os.ReadFile(filepath.Join(second, rel))
		if err != nil || string(got) != string(rune('a'+i)) {
			t.Errorf("%s = %q (err %v), want %q", rel, got, err, string(rune('a'+i)))
		}
	}
}

// TestWriteItemSharedPathOneLeaf guards #431: a path the payload names at
// several positions, under any spelling, is one file to mrp, so it is copied
// once and each position gets its marker, while a distinct file with the same
// basename keeps its own leaf.
func TestWriteItemSharedPathOneLeaf(t *testing.T) {
	dir := t.TempDir()
	shared := filepath.Join(dir, "a", "x.txt")
	other := filepath.Join(dir, "b", "x.txt")

	for _, src := range []string{shared, other} {
		if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(src, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	params := typestest.Fields(t, "fs file[]", "cfg Cfg")
	payload := map[string]any{
		"fs":  []any{shared, other, filepath.Join(dir, "a", ".", "x.txt")},
		"cfg": map[string]any{"ref": shared},
	}

	bundle := filepath.Join(dir, "bundle")
	marked := writeMarked(t, bundle, payload, params)

	want := map[string]any{
		"fs":  []any{"@mre:file:f/L0000/x.txt", "@mre:file:f/L0001/x.txt", "@mre:file:f/L0000/x.txt"},
		"cfg": map[string]any{"ref": "@mre:file:f/L0000/x.txt"},
	}
	if diff := cmp.Diff(want, marked); diff != "" {
		t.Errorf("markers (-want +got):\n%s", diff)
	}

	leaves, err := os.ReadDir(filepath.Join(bundle, "f"))
	if err != nil {
		t.Fatal(err)
	}

	if len(leaves) != 2 {
		t.Errorf("bundle holds %d leaves, want 2", len(leaves))
	}
}

// TestWriteItemSameFileAcrossBundlesOneLeaf guards #431 across bundles: a
// file a pass-through stage returns reaches the pipeline outputs through its
// producer's bundle and the pass-through's, each staged as Nextflow links it.
// mrp sees one path, so the outputs' bundle copies it once, while another
// producer's file of the same basename keeps its own leaf.
func TestWriteItemSameFileAcrossBundlesOneLeaf(t *testing.T) {
	dir := t.TempDir()
	params := typestest.Fields(t, "f txt")

	make1 := writeFileBundle(t, filepath.Join(dir, "make1"), params, filepath.Join(dir, "m1", "a.txt"))
	make2 := writeFileBundle(t, filepath.Join(dir, "make2"), params, filepath.Join(dir, "m2", "a.txt"))

	passIn := stageBundle(t, make1, filepath.Join(dir, "pass", "in"))
	pass := filepath.Join(dir, "pass", "outs")
	writeBundle(t, pass, readPayload(t, passIn), params, shim.Opts{})

	outs := typestest.Fields(t, "a txt", "b txt", "c txt")
	payload := map[string]any{"a": nil, "b": nil, "c": nil}

	for name, src := range map[string]string{"a": make1, "b": pass, "c": make2} {
		payload[name] = readPayload(t, stageBundle(t, src, filepath.Join(dir, "return", "in_"+name)))["f"]
	}

	bundle := filepath.Join(dir, "return", "args")
	marked := writeMarked(t, bundle, payload, outs)

	want := map[string]any{"a": "@mre:file:f/L0000/a.txt", "b": "@mre:file:f/L0000/a.txt", "c": "@mre:file:f/L0001/a.txt"}
	if diff := cmp.Diff(want, marked); diff != "" {
		t.Errorf("markers (-want +got):\n%s", diff)
	}

	for rel, content := range map[string]string{"f/L0000/a.txt": filepath.Join(dir, "m1", "a.txt"), "f/L0001/a.txt": filepath.Join(dir, "m2", "a.txt")} {
		if got, err := os.ReadFile(filepath.Join(bundle, rel)); err != nil || string(got) != content {
			t.Errorf("%s = %q (err %v), want %q", rel, got, err, content)
		}
	}
}

// TestLeafOriginIsWhereTheTaskWroteIt guards #432: each leaf records the path
// its producing task wrote the file at, and a pass-through stage's bundle and
// the bundle after it keep that path, so the file can be named where it was
// written.
func TestLeafOriginIsWhereTheTaskWroteIt(t *testing.T) {
	dir := t.TempDir()
	params := typestest.Fields(t, "f txt")
	written := filepath.Join(dir, "make", "main", "files", "a.txt")

	make1 := writeFileBundle(t, filepath.Join(dir, "make", "outs"), params, written)
	pass := filepath.Join(dir, "pass", "outs")
	writeBundle(t, pass, readPayload(t, stageBundle(t, make1, filepath.Join(dir, "pass", "in"))), params, shim.Opts{})

	staged := stageBundle(t, pass, filepath.Join(dir, "layout", "in"))

	for _, p := range []string{filepath.Join(make1, "f", "L0000", "a.txt"), filepath.Join(staged, "f", "L0000", "a.txt")} {
		if got, ok, err := shim.ReadOrigin(p); err != nil || !ok || got.Path != written {
			t.Errorf("ReadOrigin(%s) = %+v, %t (err %v), want %q", p, got, ok, err, written)
		}
	}
}

// TestLeafOriginRecordsHowMrpPublishesIt guards #485: a stage output that is
// a symlink, a host file outside the task and a file the run was given are
// linked into outs/ by mrp at every output that reaches them, a stage's own
// file is moved there once, and a bundle that forwards a leaf keeps how.
func TestLeafOriginRecordsHowMrpPublishesIt(t *testing.T) {
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	files := filepath.Join(work, "main", "files")
	host := writeSelfNamed(t, filepath.Join(dir, "host", "ref.txt"))
	own := writeSelfNamed(t, filepath.Join(files, "own.txt"))
	link := filepath.Join(files, "link.txt")

	if err := os.Symlink(host, link); err != nil {
		t.Fatal(err)
	}

	conf, err := shim.ConfineStage(work, "main", false, shim.Roots{})
	if err != nil {
		t.Fatal(err)
	}

	params := typestest.Fields(t, "own txt", "link txt", "host txt")
	outs := filepath.Join(work, "outs")
	writeBundle(t, outs, map[string]any{"own": own, "link": link, "host": host}, params, shim.Opts{Confine: conf})

	given := filepath.Join(dir, "given")
	writeBundle(t, given, map[string]any{"own": host}, params[:1], shim.Opts{})

	forwarded := filepath.Join(dir, "forwarded")
	writeBundle(t, forwarded, readPayload(t, stageBundle(t, outs, filepath.Join(dir, "in"))), params, shim.Opts{})

	want := map[string]shim.Origin{"own": {Path: own}, "link": {Path: link, Linked: true}, "host": {Path: host, Linked: true}}

	for _, bundle := range []string{outs, forwarded} {
		if d := cmp.Diff(want, leafOrigins(t, bundle)); d != "" {
			t.Errorf("origins in %s (-want +got):\n%s", bundle, d)
		}
	}

	if d := cmp.Diff(map[string]shim.Origin{"own": {Path: host, Linked: true}}, leafOrigins(t, given)); d != "" {
		t.Errorf("origin of a file the run was given (-want +got):\n%s", d)
	}
}

// TestLeafOriginInheritsEnclosingLeaf checks a stage output naming a file
// inside a staged directory leaf is the file mrp sees there: below a
// directory the run was given it is a host file mrp links, and below another
// stage's directory output it is that stage's file, one identity in every
// task that returns it.
func TestLeafOriginInheritsEnclosingLeaf(t *testing.T) {
	dir := t.TempDir()
	params := typestest.Fields(t, "d path")

	host := filepath.Dir(writeSelfNamed(t, filepath.Join(dir, "host", "ref", "fasta", "genome.fa")))
	given := filepath.Join(dir, "given")
	writeBundle(t, given, map[string]any{"d": filepath.Dir(host)}, params, shim.Opts{})

	producer := filepath.Join(dir, "producer")
	own := filepath.Dir(writeSelfNamed(t, filepath.Join(producer, "main", "files", "out", "sub", "x.txt")))
	produced := writeStageOuts(t, producer, map[string]any{"d": filepath.Dir(own)}, params)

	cases := map[string]struct {
		bundle, rel string
		want        shim.Origin
	}{
		"inside a given dir":    {given, "fasta/genome.fa", shim.Origin{Path: filepath.Join(host, "genome.fa"), Linked: true}},
		"inside a stage's dir":  {produced, "sub/x.txt", shim.Origin{Path: filepath.Join(own, "x.txt")}},
		"a given dir itself":    {given, "", shim.Origin{Path: filepath.Dir(host), Linked: true}},
		"a stage's dir itself":  {produced, "", shim.Origin{Path: filepath.Dir(own)}},
		"inside a given subdir": {given, "fasta", shim.Origin{Path: host, Linked: true}},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			work := filepath.Join(t.TempDir(), "work")
			staged, _ := readPayload(t, stageBundle(t, tc.bundle, filepath.Join(work, "args")))["d"].(string)
			outs := writeStageOuts(t, work, map[string]any{"d": filepath.Join(staged, tc.rel)}, params)

			if d := cmp.Diff(map[string]shim.Origin{"d": tc.want}, leafOrigins(t, outs)); d != "" {
				t.Errorf("origin (-want +got):\n%s", d)
			}
		})
	}
}

// writeStageOuts writes payload as the outs bundle of a main phase run in
// the task work dir work, and returns the bundle dir.
func writeStageOuts(t *testing.T, work string, payload map[string]any, params []*syntax.StructMember) string {
	t.Helper()

	if err := os.MkdirAll(filepath.Join(work, "main"), 0o755); err != nil {
		t.Fatal(err)
	}

	conf, err := shim.ConfineStage(work, "main", false, shim.Roots{})
	if err != nil {
		t.Fatal(err)
	}

	outs := filepath.Join(work, "outs")
	writeBundle(t, outs, payload, params, shim.Opts{Confine: conf})

	return outs
}

// leafOrigins maps each file leaf of the bundle at dir to its origin.
func leafOrigins(t *testing.T, dir string) map[string]shim.Origin {
	t.Helper()

	return payloadOrigins(t, readPayload(t, dir))
}

// payloadOrigins maps each file leaf of a payload read back to its origin.
func payloadOrigins(t *testing.T, payload map[string]any) map[string]shim.Origin {
	t.Helper()

	out := map[string]shim.Origin{}

	for name, v := range payload {
		leaf, _ := v.(string)

		o, ok, err := shim.ReadOrigin(leaf)
		if err != nil || !ok {
			t.Fatalf("origin of %s at %s: %v (recorded %v)", name, leaf, err, ok)
		}

		out[name] = o
	}

	return out
}

// writeSelfNamed writes a file at path holding its path, and returns path.
func writeSelfNamed(t *testing.T, path string) string {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(path), 0o644); err != nil {
		t.Fatal(err)
	}

	return path
}

// linkLeaves links payload's leaves below dir and returns it read back.
func linkLeaves(t *testing.T, dir string, payload map[string]any, params []*syntax.StructMember, roots shim.Roots) map[string]any {
	t.Helper()

	raw, err := shim.LinkLeaves(dir, payload, params, fileTable(t), roots)
	if err != nil {
		t.Fatalf("LinkLeaves: %v", err)
	}

	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}

	return out
}

// TestLinkLeaves: linked leaves hold no copy of a file they name (#498),
// and no payload file. A leaf of a staged bundle is linked by its ordinal
// dir, origin record and all, and a host file by itself, its origin
// recorded beside it; both read back as the files, with the origins a
// copying bundle records, at their links, as ReadBundle names a bundle's
// leaves.
func TestLinkLeaves(t *testing.T) {
	dir := t.TempDir()
	one := typestest.Fields(t, "f file")
	params := typestest.Fields(t, "host file", "up file")

	host := writeSelfNamed(t, filepath.Join(dir, "host", "ref.txt"))
	own := writeSelfNamed(t, filepath.Join(dir, "producer", "main", "files", "y.txt"))
	produced := writeStageOuts(t, filepath.Join(dir, "producer"), map[string]any{"f": own}, one)
	staged, _ := readPayload(t, stageBundle(t, produced, filepath.Join(dir, "task", "in", "u0")))["f"].(string)

	args := filepath.Join(dir, "task", "args")
	linked := linkLeaves(t, args, map[string]any{"host": host, "up": staged}, params, shim.Roots{})

	var files []string

	err := filepath.WalkDir(args, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			files = append(files, d.Name())
		}

		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	if d := cmp.Diff([]string{".mre-origin"}, files); d != "" {
		t.Errorf("files the args dir holds (-want +got):\n%s", d)
	}

	want := map[string]shim.Origin{"host": {Path: host, Linked: true}, "up": {Path: own}}
	if d := cmp.Diff(want, payloadOrigins(t, linked)); d != "" {
		t.Errorf("origins (-want +got):\n%s", d)
	}

	for name, src := range map[string]string{"host": host, "up": own} {
		leaf, _ := linked[name].(string)
		if got, err := os.ReadFile(leaf); err != nil || string(got) != src {
			t.Errorf("%s reads %q (%v), want %s's bytes", name, got, err, src)
		}

		if !strings.HasPrefix(leaf, args+string(filepath.Separator)) {
			t.Errorf("%s is named %s, want its link below %s", name, leaf, args)
		}
	}
}

// TestLinkLeavesFails: linking fails loudly where it cannot place a leaf's
// link, whether the files dir is blocked or a link already holds the
// leaf's place, for a host file and for a staged bundle's leaf.
func TestLinkLeavesFails(t *testing.T) {
	dir := t.TempDir()
	one := typestest.Fields(t, "f file")

	host := writeSelfNamed(t, filepath.Join(dir, "host", "ref.txt"))
	own := writeSelfNamed(t, filepath.Join(dir, "producer", "main", "files", "y.txt"))
	produced := writeStageOuts(t, filepath.Join(dir, "producer"), map[string]any{"f": own}, one)
	staged, _ := readPayload(t, stageBundle(t, produced, filepath.Join(dir, "task", "in", "u0")))["f"].(string)

	for name, leaf := range map[string]string{"host file": host, "staged leaf": staged} {
		t.Run(name, func(t *testing.T) {
			payload := map[string]any{"f": leaf}

			blocked := filepath.Join(t.TempDir(), "args")
			if err := os.MkdirAll(blocked, 0o755); err != nil {
				t.Fatal(err)
			}

			if err := os.WriteFile(filepath.Join(blocked, "f"), nil, 0o644); err != nil {
				t.Fatal(err)
			}

			if _, err := shim.LinkLeaves(blocked, payload, one, fileTable(t), shim.Roots{}); err == nil {
				t.Error("LinkLeaves with the files dir blocked succeeded")
			}

			twice := filepath.Join(t.TempDir(), "args")
			linkLeaves(t, twice, payload, one, shim.Roots{})

			if _, err := shim.LinkLeaves(twice, payload, one, fileTable(t), shim.Roots{}); err == nil {
				t.Error("LinkLeaves over a dir that holds the leaf's link succeeded")
			}
		})
	}
}

// TestRealPathOfLinkedArgsStages: a stage that returns the real path of a
// file its linked args name, an upstream stage's file or an entry file, names
// a file outside its task. Under either confinement it stages as the args
// file it resolves from does, hard-linked with that file's origin: mrp's
// stage returns the same file either way.
func TestRealPathOfLinkedArgsStages(t *testing.T) {
	dir := t.TempDir()
	one := typestest.Fields(t, "f file")
	params := typestest.Fields(t, "host file", "up file")

	host := writeSelfNamed(t, filepath.Join(dir, "host", "ref.txt"))
	own := writeSelfNamed(t, filepath.Join(dir, "producer", "main", "files", "y.txt"))
	produced := writeStageOuts(t, filepath.Join(dir, "producer"), map[string]any{"f": own}, one)

	task := filepath.Join(dir, "task")
	entry := filepath.Join(task, "entry", "1", "ref.txt")

	for _, d := range []string{filepath.Join(task, "in"), filepath.Dir(entry), filepath.Join(task, "main")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	if err := errors.Join(os.Symlink(produced, filepath.Join(task, "in", "u0")), os.Symlink(host, entry)); err != nil {
		t.Fatal(err)
	}

	staged, _ := readPayload(t, filepath.Join(task, "in", "u0"))["f"].(string)
	resolved := map[string]any{}

	for name, v := range linkLeaves(t, filepath.Join(task, "args"), map[string]any{"host": entry, "up": staged}, params, shim.Roots{}) {
		leaf, _ := v.(string)

		r, err := filepath.EvalSymlinks(leaf)
		if err != nil {
			t.Fatal(err)
		}

		resolved[name] = r
	}

	realHost, err := filepath.EvalSymlinks(host)
	if err != nil {
		t.Fatal(err)
	}

	for _, strict := range []bool{true, false} {
		conf, err := shim.ConfineStage(task, "main", strict, shim.Roots{})
		if err != nil {
			t.Fatal(err)
		}

		outs := filepath.Join(dir, "outs", strconv.FormatBool(strict))
		if _, err := shim.WriteItem(outs, resolved, params, fileTable(t), shim.Opts{Confine: conf}, shim.Staged{}); err != nil {
			t.Fatalf("strict %v: WriteItem of the real paths of linked args: %v", strict, err)
		}

		want := map[string]shim.Origin{"host": {Path: realHost, Linked: true}, "up": {Path: own}}
		if d := cmp.Diff(want, leafOrigins(t, outs)); d != "" {
			t.Errorf("strict %v: origins (-want +got):\n%s", strict, d)
		}

		leaf, _ := readPayload(t, outs)["up"].(string)
		if !sameInode(t, leaf, own) {
			t.Errorf("strict %v: %s is not a hard link to the upstream file %s", strict, leaf, own)
		}
	}
}

func sameInode(t *testing.T, a, b string) bool {
	t.Helper()

	ai, err := os.Stat(a)
	if err != nil {
		t.Fatal(err)
	}

	bi, err := os.Stat(b)
	if err != nil {
		t.Fatal(err)
	}

	return os.SameFile(ai, bi)
}

// TestWriteItemRefusesOriginFileName checks a leaf named like the origin file
// it would share its ordinal dir with fails the write rather than overwrite
// either.
func TestWriteItemRefusesOriginFileName(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, ".mre-origin")

	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	params := typestest.Fields(t, "f file")

	_, err := shim.WriteItem(filepath.Join(dir, "bundle"), map[string]any{"f": src}, params, fileTable(t), shim.Opts{}, shim.Staged{})
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Errorf("WriteItem of a leaf named .mre-origin = %v, want a reserved-name error", err)
	}
}

// writeFileBundle writes a file at src holding its own path and a bundle at
// dir whose param f names it, and returns dir.
func writeFileBundle(t *testing.T, dir string, params []*syntax.StructMember, src string) string {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(src, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	writeBundle(t, dir, map[string]any{"f": src}, params, shim.Opts{})

	return dir
}

// stageBundle stages the bundle at src into dst as a Nextflow process input
// does, data.json and a link to each leaf dir, and returns dst.
func stageBundle(t *testing.T, src, dst string) string {
	t.Helper()

	if err := os.MkdirAll(filepath.Join(dst, "f"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(filepath.Join(src, "data.json"), filepath.Join(dst, "data.json")); err != nil {
		t.Fatal(err)
	}

	leaves, err := os.ReadDir(filepath.Join(src, "f"))
	if err != nil {
		t.Fatal(err)
	}

	for _, leaf := range leaves {
		if err := os.Symlink(filepath.Join(src, "f", leaf.Name()), filepath.Join(dst, "f", leaf.Name())); err != nil {
			t.Fatal(err)
		}
	}

	return dst
}

// readPayload reads the bundle at dir as a stage or bind reads it.
func readPayload(t *testing.T, dir string) map[string]any {
	t.Helper()

	raw, err := shim.Refs{}.ReadBundle(dir)
	if err != nil {
		t.Fatalf("ReadBundle %s: %v", dir, err)
	}

	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}

	return payload
}

// TestBundleRoundTrip writes a payload with a file leaf into a bundle, then
// reads it back from a different relative path, proving the file travels with
// the payload and the marker resolves to a real absolute path.
func TestBundleRoundTrip(t *testing.T) {
	dir := t.TempDir()

	// A source file the payload references by absolute path.
	srcDir := filepath.Join(dir, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}

	src := filepath.Join(srcDir, "note.txt")
	if err := os.WriteFile(src, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	params := typestest.Fields(t, "f file", "n int")
	payload := map[string]any{"f": src, "n": float64(7)}

	bundle := filepath.Join(dir, "bundle")
	writeBundle(t, bundle, payload, params, shim.Opts{})

	raw, err := os.ReadFile(filepath.Join(bundle, "data.json"))
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(raw), "@mre:file:f/L0000/note.txt") {
		t.Errorf("data.json should contain an ordinal file marker, got: %s", raw)
	}

	got := readPayload(t, bundle)

	path, ok := got["f"].(string)
	if !ok || !filepath.IsAbs(path) || filepath.Base(path) != "note.txt" {
		t.Fatalf("resolved f = %v, want an absolute path ending in note.txt", got["f"])
	}

	content, err := os.ReadFile(path)
	if err != nil || string(content) != "hello" {
		t.Errorf("resolved file content = %q (err %v), want hello", content, err)
	}

	if got["n"] != float64(7) {
		t.Errorf("non-file value n = %v, want 7", got["n"])
	}
}

// TestWriteItemDirLeaf guards #43: a directory leaf is staged with DirMarker so
// its ground-truth dir-ness (stat at staging time) travels to publish, rather
// than being inferred from the declared output type. A plain-file leaf keeps
// FileMarker; both resolve to a real absolute path on read-back.
func TestWriteItemDirLeaf(t *testing.T) {
	dir := t.TempDir()

	// A directory the payload references, declared with a plain `file` type — the
	// mistyped-directory shape #43 is about.
	srcDir := filepath.Join(dir, "outdir")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "part.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	params := typestest.Fields(t, "d file")
	payload := map[string]any{"d": srcDir}

	marked := writeMarked(t, dir, payload, params)

	got, ok := marked["d"].(string)
	if !ok {
		t.Fatalf("marked d = %v, want a marker string", marked["d"])
	}

	rel, isDir, ok := shim.CutMarker(got)
	if !ok || !isDir {
		t.Errorf("CutMarker(%q) = %q, %v, %v; want a dir marker", got, rel, isDir, ok)
	}

	// The staged leaf is a directory carrying the original tree.
	if fi, err := os.Stat(filepath.Join(dir, rel)); err != nil || !fi.IsDir() {
		t.Errorf("staged leaf %s should be a directory (err %v)", rel, err)
	}
}

// TestWriteItemNamesDirSpelledWithSuffix checks a directory leaf spelled with a
// trailing "/" or "/." keeps the directory's own name, so its marker is valid,
// and that both spellings name one leaf.
func TestWriteItemNamesDirSpelledWithSuffix(t *testing.T) {
	dir := t.TempDir()

	srcDir := filepath.Join(dir, "outdir")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}

	params := typestest.Fields(t, "d path[]")
	payload := map[string]any{"d": []any{srcDir + "/", srcDir + "/."}}

	marked := writeMarked(t, filepath.Join(dir, "bundle"), payload, params)

	want := map[string]any{"d": []any{"@mre:dir:f/L0000/outdir", "@mre:dir:f/L0000/outdir"}}
	if diff := cmp.Diff(want, marked); diff != "" {
		t.Errorf("markers (-want +got):\n%s", diff)
	}
}

// TestMarkerRoundTrip pins the marker encode/decode contract both ways.
func TestMarkerRoundTrip(t *testing.T) {
	cases := []struct {
		rel   string
		isDir bool
	}{
		{"f/L0000/reads.txt", false},
		{"f/L0001/ref", true},
	}

	for _, c := range cases {
		m := shim.Marker(c.rel, c.isDir)

		rel, isDir, ok := shim.CutMarker(m)
		if !ok || rel != c.rel || isDir != c.isDir {
			t.Errorf("CutMarker(Marker(%q,%v)) = %q,%v,%v", c.rel, c.isDir, rel, isDir, ok)
		}
	}

	if _, _, ok := shim.CutMarker("/plain/path"); ok {
		t.Error("CutMarker on a non-marker string should report ok=false")
	}
}

// TestBundleKeepsMarkerShapedStrings guards #328: a string value spelled like a
// transport marker must come back from the bundle as the same string, wherever
// it sits (a string param, a string array, an untyped map, a key no param
// declares, or a file-typed leaf naming no file), exactly as mrp hands it to the
// next stage. The rels name the bundle's real leaf, so resolving any of them
// would hand a stage another file's path in place of its string.
func TestBundleKeepsMarkerShapedStrings(t *testing.T) {
	dir := t.TempDir()

	src := filepath.Join(dir, "made.txt")
	if err := os.WriteFile(src, []byte("made"), 0o644); err != nil {
		t.Fatal(err)
	}

	params := typestest.Fields(t, "made txt", "fake txt", "s string", "arr string[]", "m map")
	strs := []any{
		"@mre:file:f/L0000/made.txt",
		"@mre:dir:f/L0000/made.txt",
		"@mre:str:@mre:file:f/L0000/made.txt",
		"@mre:",
		"@mre:file:../elsewhere",
		"plain",
	}
	payload := map[string]any{
		"made":  src,
		"fake":  "@mre:file:f/L0000/made.txt",
		"s":     "@mre:file:f/L0000/made.txt",
		"arr":   strs,
		"m":     map[string]any{"k": "@mre:dir:f/L0000/made.txt", "nested": strs},
		"extra": "@mre:file:f/L0000/made.txt",
	}

	bundle := filepath.Join(dir, "bundle")
	writeBundle(t, bundle, payload, params, shim.Opts{})

	got := readPayload(t, bundle)

	abs, err := filepath.Abs(bundle)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]any{
		"made":  filepath.Join(abs, "f", "L0000", "made.txt"),
		"fake":  payload["fake"],
		"s":     payload["s"],
		"arr":   strs,
		"m":     payload["m"],
		"extra": payload["extra"],
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("bundle round trip changed a marker-shaped string (-want +got):\n%s", diff)
	}
}

// TestReadBundleRejectsMalformedMarkers guards #328's read side: mre writes a
// marker only as f/L<ordinal>/<basename> inside the bundle and escapes every
// other reserved string, so a payload string carrying the reserved prefix in any
// other shape (a rel leaving the bundle, an absolute rel, no basename or more
// than one name under the ordinal, an unknown kind) is a corrupt or forged
// bundle and fails the read instead of resolving to a path.
func TestReadBundleRejectsMalformedMarkers(t *testing.T) {
	bad := []string{
		"@mre:file:../elsewhere",
		"@mre:dir:/abs/path",
		"@mre:file:f/../../elsewhere",
		"@mre:file:f/L12/a.txt",
		"@mre:file:g/L0000/a.txt",
		"@mre:file:f/L0000",
		"@mre:file:f/L0000.txt",
		"@mre:file:f/L0000/",
		"@mre:file:f/L0000/.",
		"@mre:file:f/L0000/..",
		"@mre:file:f/L0000/../../x",
		"@mre:file:f/L0000/sub/a.txt",
		"@mre:file:f/L0000/a\x00b",
		"@mre:file:f/L0000a/b",
		"@mre:other:x",
		"@mre:",
	}

	for _, s := range bad {
		t.Run(s, func(t *testing.T) {
			dir := t.TempDir()
			data, err := json.Marshal(map[string]any{"v": []any{s}})
			if err != nil {
				t.Fatal(err)
			}

			if err := os.WriteFile(filepath.Join(dir, "data.json"), data, 0o644); err != nil {
				t.Fatal(err)
			}

			if _, err := (shim.Refs{}).ReadBundle(dir); !errors.Is(err, shim.ErrBadMarker) {
				t.Errorf("ReadBundle(%q) err = %v, want ErrBadMarker", s, err)
			}
		})
	}

	good := map[string]string{
		"@mre:file:f/L0000/reads":           "f/L0000/reads",
		"@mre:file:f/L12345/a.tar.gz":       "f/L12345/a.tar.gz",
		"@mre:dir:f/L0001/ref":              "f/L0001/ref",
		"@mre:file:f/L0002/.hidden":         "f/L0002/.hidden",
		"@mre:file:f/L0003/...":             "f/L0003/...",
		"@mre:file:f/L0004/sp ace;$x*?.bam": "f/L0004/sp ace;$x*?.bam",
	}

	for s, rel := range good {
		if got, _, ok := shim.CutMarker(s); !ok || got != rel {
			t.Errorf("CutMarker(%q) = %q, %v; want %q, true", s, got, ok, rel)
		}
	}
}

// TestBundleNestedStructFile checks a file nested inside a struct leaf travels.
func TestBundleNestedStructFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "deep.bam")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	payload := map[string]any{"c": map[string]any{"ref": src}}
	params := typestest.Fields(t, "c Cfg")

	bundle := filepath.Join(dir, "b")
	writeBundle(t, bundle, payload, params, shim.Opts{})

	got := readPayload(t, bundle)

	inner, ok := got["c"].(map[string]any)
	if !ok {
		t.Fatalf("c = %v, want a struct object", got["c"])
	}

	ref, ok := inner["ref"].(string)
	if !ok {
		t.Fatalf("c.ref = %v, want a path string", inner["ref"])
	}

	if content, err := os.ReadFile(ref); err != nil || string(content) != "x" {
		t.Errorf("nested file content = %q (err %v), want x", content, err)
	}
}

// TestBundleMissingFileKeepsPath guards bug 4: a declared output file the stage
// legitimately did not write must keep its path string at bundle time, not error
// (CopyTree's stat) or null. Martian keeps the path as-is at stage finalize and
// only resolves an absent file to null at publish (core/stage.go, post_process.go).
// A downstream join (e.g. MAKE_SHARD read_prefix_counts) needs the path string.
func TestBundleMissingFileKeepsPath(t *testing.T) {
	dir := t.TempDir()

	missing := filepath.Join(dir, "never-written.txt")
	params := typestest.Fields(t, "f file", "n int")
	payload := map[string]any{"f": missing, "n": float64(3)}

	marked := writeMarked(t, dir, payload, params)

	if marked["f"] != missing {
		t.Errorf("absent output f = %v, want the unchanged path %q", marked["f"], missing)
	}
	if marked["n"] != float64(3) {
		t.Errorf("non-file value n = %v, want 3", marked["n"])
	}
}

// TestReadBundleEmpty returns nil for an absent optional input.
func TestReadBundleEmpty(t *testing.T) {
	got, err := shim.Refs{}.ReadBundle("")
	if err != nil || got != nil {
		t.Errorf("ReadBundle(\"\") = %v, %v; want nil, nil", got, err)
	}
}

// TestCopyTreeDerefSymlink is the regression guard for the entry-file isolation
// fault: Nextflow stages inputs as symlinks, and link(2) does not follow them, so
// hard-linking a symlinked source would copy a dangling link into the bundle —
// which then breaks once the bundle is staged into another isolated task. The
// destination must be a real file carrying the target's content, not a symlink.
func TestCopyTreeDerefSymlink(t *testing.T) {
	dir := t.TempDir()

	target := filepath.Join(dir, "real.txt")
	if err := os.WriteFile(target, []byte("staged content"), 0o644); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(dir, "staged_link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(dir, "out", "copied.txt")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil { // CopyTree's caller makes the parent
		t.Fatal(err)
	}
	if err := shim.CopyTree(link, dst); err != nil {
		t.Fatalf("CopyTree: %v", err)
	}

	// The copy must not itself be a symlink (a dangling link in the next task).
	lst, err := os.Lstat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if lst.Mode()&os.ModeSymlink != 0 {
		t.Error("CopyTree produced a symlink; it must dereference to a real file")
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "staged content" {
		t.Errorf("copied content = %q, want %q", got, "staged content")
	}
}

// TestCopyTreeDanglingSymlinkInDir checks that a dangling symlink INSIDE a copied
// directory is treated as absent (skipped), not aborting the whole bundle — the
// same tolerance WriteItem's top-level guard gives a dangling output path.
func TestCopyTreeDanglingSymlinkInDir(t *testing.T) {
	dir := t.TempDir()

	src := filepath.Join(dir, "out")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "real.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "gone"), filepath.Join(src, "broken")); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(dir, "copy")
	if err := shim.CopyTree(src, dst); err != nil {
		t.Fatalf("CopyTree with a dangling symlink child must skip it, not error: %v", err)
	}

	if got, err := os.ReadFile(filepath.Join(dst, "real.txt")); err != nil || string(got) != "keep" {
		t.Errorf("real sibling not copied: %q, %v", got, err)
	}
	// The dangling link must not have been reproduced (it would dangle in the next
	// isolated task) and must not have aborted the copy.
	if _, err := os.Lstat(filepath.Join(dst, "broken")); !os.IsNotExist(err) {
		t.Errorf("dangling symlink child was reproduced in the copy: %v", err)
	}
}

// TestCopyTreeReadOnlyDir checks a read-only (0555) source directory is still
// copied: the destination must be created writable so its children can be
// written, then restored to the source mode.
func TestCopyTreeReadOnlyDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory write permissions, so 0555 would not exercise the fix")
	}

	dir := t.TempDir()

	src := filepath.Join(dir, "ro")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "child.txt"), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(src, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(src, 0o755) }) // let t.TempDir cleanup remove it

	dst := filepath.Join(dir, "copy")
	t.Cleanup(func() { _ = os.Chmod(dst, 0o755) }) // 0555 copy would block TempDir cleanup
	if err := shim.CopyTree(src, dst); err != nil {
		t.Fatalf("CopyTree of a read-only source dir: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dst, "child.txt"))
	if err != nil || string(got) != "payload" {
		t.Fatalf("copied child = %q, %v; want %q", got, err, "payload")
	}

	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o555 {
		t.Errorf("copied dir mode = %o, want 0555 (source mode restored)", info.Mode().Perm())
	}
}

// TestReadBundleMissingData checks the failure arm for a bundle directory with no
// data.json payload.
func TestReadBundleMissingData(t *testing.T) {
	if _, err := (shim.Refs{}).ReadBundle(t.TempDir()); err == nil {
		t.Fatal("ReadBundle of a dir without data.json must error")
	}
}

// TestReadBundleCorruptJSON checks the failure arm for a data.json that is not
// valid JSON — a silent nil here would drop the payload.
func TestReadBundleCorruptJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "data.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := (shim.Refs{}).ReadBundle(dir); err == nil {
		t.Fatal("ReadBundle of corrupt data.json must error")
	}
}

// TestWriteItemDanglingSymlink checks the documented dangling-symlink-as-absent
// behavior: a declared output that is a symlink to a nonexistent target stats as
// absent (os.Stat follows the link), so WriteItem keeps the path unchanged rather
// than aborting the bundle.
func TestWriteItemDanglingSymlink(t *testing.T) {
	dir := t.TempDir()

	dangling := filepath.Join(dir, "dangling")
	if err := os.Symlink(filepath.Join(dir, "nonexistent-target"), dangling); err != nil {
		t.Fatal(err)
	}

	params := typestest.Fields(t, "f file")
	marked := writeMarked(t, dir, map[string]any{"f": dangling}, params)
	if marked["f"] != dangling {
		t.Errorf("dangling output f = %v, want the unchanged path %q", marked["f"], dangling)
	}
}
