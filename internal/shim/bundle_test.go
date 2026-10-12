package shim_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/eunmann/mro2nf/internal/types"
	"github.com/google/go-cmp/cmp"
)

func fileTable() *types.Table {
	return types.NewTable(map[string]*ir.StructType{
		"Cfg": {Name: "Cfg", Fields: []ir.Param{{Name: "ref", BaseType: "file", IsFile: true}}},
	})
}

// TestMarkFilesKeepsBasename guards #284: a stage reads sample names and
// extensions from its inputs' basenames, so each leaf keeps the basename mrp
// would pass, under its own ordinal dir so leaves sharing a basename never
// collide. A second bundle written from the first's resolved paths keeps it too.
func TestMarkFilesKeepsBasename(t *testing.T) {
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

	params := []ir.Param{{Name: "fs", BaseType: "fastq.gz", IsFile: true, ArrayDim: 1}}
	first := filepath.Join(dir, "first")
	if err := shim.WriteBundle(first, map[string]any{"fs": srcs}, params, fileTable(), nil); err != nil {
		t.Fatalf("WriteBundle: %v", err)
	}

	resolved, err := shim.ReadBundle(first)
	if err != nil {
		t.Fatalf("ReadBundle: %v", err)
	}

	var mid map[string]any
	if err := json.Unmarshal(resolved, &mid); err != nil {
		t.Fatal(err)
	}

	second := filepath.Join(dir, "second")
	marked, err := shim.MarkFiles(second, mid, params, fileTable(), nil)
	if err != nil {
		t.Fatalf("MarkFiles: %v", err)
	}

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

// TestMarkFilesSharedPathOneLeaf guards #431: a path the payload names at
// several positions, under any spelling, is one file to mrp, so it is copied
// once and each position gets its marker, while a distinct file with the same
// basename keeps its own leaf.
func TestMarkFilesSharedPathOneLeaf(t *testing.T) {
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

	params := []ir.Param{
		{Name: "fs", BaseType: "file", IsFile: true, ArrayDim: 1},
		{Name: "cfg", BaseType: "Cfg", IsFile: true},
	}
	payload := map[string]any{
		"fs":  []any{shared, other, filepath.Join(dir, "a", ".", "x.txt")},
		"cfg": map[string]any{"ref": shared},
	}

	bundle := filepath.Join(dir, "bundle")

	marked, err := shim.MarkFiles(bundle, payload, params, fileTable(), nil)
	if err != nil {
		t.Fatalf("MarkFiles: %v", err)
	}

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

// TestMarkFilesSameFileAcrossBundlesOneLeaf guards #431 across bundles: a
// file a pass-through stage returns reaches the pipeline outputs through its
// producer's bundle and the pass-through's, each staged as Nextflow links it.
// mrp sees one path, so the outputs' bundle copies it once, while another
// producer's file of the same basename keeps its own leaf.
func TestMarkFilesSameFileAcrossBundlesOneLeaf(t *testing.T) {
	dir := t.TempDir()
	params := []ir.Param{{Name: "f", BaseType: "txt", IsFile: true}}

	make1 := writeFileBundle(t, filepath.Join(dir, "make1"), params, filepath.Join(dir, "m1", "a.txt"))
	make2 := writeFileBundle(t, filepath.Join(dir, "make2"), params, filepath.Join(dir, "m2", "a.txt"))

	passIn := stageBundle(t, make1, filepath.Join(dir, "pass", "in"))
	pass := filepath.Join(dir, "pass", "outs")

	if err := shim.WriteBundle(pass, readPayload(t, passIn), params, fileTable(), nil); err != nil {
		t.Fatalf("WriteBundle pass-through: %v", err)
	}

	outs := []ir.Param{
		{Name: "a", BaseType: "txt", IsFile: true},
		{Name: "b", BaseType: "txt", IsFile: true},
		{Name: "c", BaseType: "txt", IsFile: true},
	}
	payload := map[string]any{"a": nil, "b": nil, "c": nil}

	for name, src := range map[string]string{"a": make1, "b": pass, "c": make2} {
		payload[name] = readPayload(t, stageBundle(t, src, filepath.Join(dir, "return", "in_"+name)))["f"]
	}

	bundle := filepath.Join(dir, "return", "args")

	marked, err := shim.MarkFiles(bundle, payload, outs, fileTable(), nil)
	if err != nil {
		t.Fatalf("MarkFiles: %v", err)
	}

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
	params := []ir.Param{{Name: "f", BaseType: "txt", IsFile: true}}
	written := filepath.Join(dir, "make", "main", "files", "a.txt")

	make1 := writeFileBundle(t, filepath.Join(dir, "make", "outs"), params, written)
	pass := filepath.Join(dir, "pass", "outs")

	if err := shim.WriteBundle(pass, readPayload(t, stageBundle(t, make1, filepath.Join(dir, "pass", "in"))), params, fileTable(), nil); err != nil {
		t.Fatalf("WriteBundle pass-through: %v", err)
	}

	staged := stageBundle(t, pass, filepath.Join(dir, "layout", "in"))

	for _, p := range []string{filepath.Join(make1, "f", "L0000", "a.txt"), filepath.Join(staged, "f", "L0000", "a.txt")} {
		if got, err := shim.LeafOrigin(p); err != nil || got != written {
			t.Errorf("LeafOrigin(%s) = %q (err %v), want %q", p, got, err, written)
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

	conf, err := shim.ConfineStage(work, "main", false)
	if err != nil {
		t.Fatal(err)
	}

	params := []ir.Param{{Name: "own", BaseType: "txt", IsFile: true}, {Name: "link", BaseType: "txt", IsFile: true}, {Name: "host", BaseType: "txt", IsFile: true}}
	outs := filepath.Join(work, "outs")

	if err := shim.WriteBundle(outs, map[string]any{"own": own, "link": link, "host": host}, params, fileTable(), conf); err != nil {
		t.Fatal(err)
	}

	given := filepath.Join(dir, "given")
	if err := shim.WriteBundle(given, map[string]any{"own": host}, params[:1], fileTable(), nil); err != nil {
		t.Fatal(err)
	}

	forwarded := filepath.Join(dir, "forwarded")
	if err := shim.WriteBundle(forwarded, readPayload(t, stageBundle(t, outs, filepath.Join(dir, "in"))), params, fileTable(), nil); err != nil {
		t.Fatal(err)
	}

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
	params := []ir.Param{{Name: "d", BaseType: "path", IsFile: true}}

	host := filepath.Dir(writeSelfNamed(t, filepath.Join(dir, "host", "ref", "fasta", "genome.fa")))
	given := filepath.Join(dir, "given")

	if err := shim.WriteBundle(given, map[string]any{"d": filepath.Dir(host)}, params, fileTable(), nil); err != nil {
		t.Fatal(err)
	}

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
func writeStageOuts(t *testing.T, work string, payload map[string]any, params []ir.Param) string {
	t.Helper()

	if err := os.MkdirAll(filepath.Join(work, "main"), 0o755); err != nil {
		t.Fatal(err)
	}

	conf, err := shim.ConfineStage(work, "main", false)
	if err != nil {
		t.Fatal(err)
	}

	outs := filepath.Join(work, "outs")
	if err := shim.WriteBundle(outs, payload, params, fileTable(), conf); err != nil {
		t.Fatal(err)
	}

	return outs
}

// leafOrigins maps each file leaf of the bundle at dir to its origin.
func leafOrigins(t *testing.T, dir string) map[string]shim.Origin {
	t.Helper()

	out := map[string]shim.Origin{}

	for name, v := range readPayload(t, dir) {
		leaf, _ := v.(string)

		o, ok, err := shim.ReadOrigin(leaf)
		if err != nil || !ok {
			t.Fatalf("origin of %s in %s: %v (recorded %v)", name, dir, err, ok)
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

// TestMarkFilesRefusesOriginFileName checks a leaf named like the origin file
// it would share its ordinal dir with fails the write rather than overwrite
// either.
func TestMarkFilesRefusesOriginFileName(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, ".mre-origin")

	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	params := []ir.Param{{Name: "f", BaseType: "file", IsFile: true}}

	_, err := shim.MarkFiles(filepath.Join(dir, "bundle"), map[string]any{"f": src}, params, fileTable(), nil)
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Errorf("MarkFiles of a leaf named .mre-origin = %v, want a reserved-name error", err)
	}
}

// writeFileBundle writes a file at src holding its own path and a bundle at
// dir whose param f names it, and returns dir.
func writeFileBundle(t *testing.T, dir string, params []ir.Param, src string) string {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(src, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := shim.WriteBundle(dir, map[string]any{"f": src}, params, fileTable(), nil); err != nil {
		t.Fatalf("WriteBundle %s: %v", dir, err)
	}

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

	raw, err := shim.ReadBundle(dir)
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

	params := []ir.Param{
		{Name: "f", BaseType: "file", IsFile: true},
		{Name: "n", BaseType: "int"},
	}
	payload := map[string]any{"f": src, "n": float64(7)}

	bundle := filepath.Join(dir, "bundle")
	if err := shim.WriteBundle(bundle, payload, params, fileTable(), nil); err != nil {
		t.Fatalf("WriteBundle: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(bundle, "data.json"))
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(raw), "@mre:file:f/L0000/note.txt") {
		t.Errorf("data.json should contain an ordinal file marker, got: %s", raw)
	}

	resolved, err := shim.ReadBundle(bundle)
	if err != nil {
		t.Fatalf("ReadBundle: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(resolved, &got); err != nil {
		t.Fatal(err)
	}

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

// TestMarkFilesDirLeaf guards #43: a directory leaf is staged with DirMarker so
// its ground-truth dir-ness (stat at staging time) travels to publish, rather
// than being inferred from the declared output type. A plain-file leaf keeps
// FileMarker; both resolve to a real absolute path on read-back.
func TestMarkFilesDirLeaf(t *testing.T) {
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

	params := []ir.Param{{Name: "d", BaseType: "file", IsFile: true}}
	payload := map[string]any{"d": srcDir}

	marked, err := shim.MarkFiles(dir, payload, params, fileTable(), nil)
	if err != nil {
		t.Fatalf("MarkFiles: %v", err)
	}

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

// TestMarkFilesNamesDirSpelledWithSuffix checks a directory leaf spelled with a
// trailing "/" or "/." keeps the directory's own name, so its marker is valid,
// and that both spellings name one leaf.
func TestMarkFilesNamesDirSpelledWithSuffix(t *testing.T) {
	dir := t.TempDir()

	srcDir := filepath.Join(dir, "outdir")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}

	params := []ir.Param{{Name: "d", BaseType: "path", IsFile: true, ArrayDim: 1}}
	payload := map[string]any{"d": []any{srcDir + "/", srcDir + "/."}}

	marked, err := shim.MarkFiles(filepath.Join(dir, "bundle"), payload, params, fileTable(), nil)
	if err != nil {
		t.Fatalf("MarkFiles: %v", err)
	}

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

	params := []ir.Param{
		{Name: "made", BaseType: "txt", IsFile: true},
		{Name: "fake", BaseType: "txt", IsFile: true},
		{Name: "s", BaseType: "string"},
		{Name: "arr", BaseType: "string", ArrayDim: 1},
		{Name: "m", BaseType: "map"},
	}
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
	if err := shim.WriteBundle(bundle, payload, params, fileTable(), nil); err != nil {
		t.Fatalf("WriteBundle: %v", err)
	}

	resolved, err := shim.ReadBundle(bundle)
	if err != nil {
		t.Fatalf("ReadBundle: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(resolved, &got); err != nil {
		t.Fatal(err)
	}

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

			if _, err := shim.ReadBundle(dir); !errors.Is(err, shim.ErrBadMarker) {
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
	params := []ir.Param{{Name: "c", BaseType: "Cfg"}}

	bundle := filepath.Join(dir, "b")
	if err := shim.WriteBundle(bundle, payload, params, fileTable(), nil); err != nil {
		t.Fatalf("WriteBundle: %v", err)
	}

	resolved, err := shim.ReadBundle(bundle)
	if err != nil {
		t.Fatalf("ReadBundle: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(resolved, &got); err != nil {
		t.Fatal(err)
	}

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
	params := []ir.Param{
		{Name: "f", BaseType: "file", IsFile: true},
		{Name: "n", BaseType: "int"},
	}
	payload := map[string]any{"f": missing, "n": float64(3)}

	marked, err := shim.MarkFiles(dir, payload, params, fileTable(), nil)
	if err != nil {
		t.Fatalf("MarkFiles must not error on an absent declared output: %v", err)
	}

	if marked["f"] != missing {
		t.Errorf("absent output f = %v, want the unchanged path %q", marked["f"], missing)
	}
	if marked["n"] != float64(3) {
		t.Errorf("non-file value n = %v, want 3", marked["n"])
	}
}

// TestReadBundleEmpty returns nil for an absent optional input.
func TestReadBundleEmpty(t *testing.T) {
	got, err := shim.ReadBundle("")
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
// same tolerance MarkFiles' top-level guard gives a dangling output path.
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
	if _, err := shim.ReadBundle(t.TempDir()); err == nil {
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

	if _, err := shim.ReadBundle(dir); err == nil {
		t.Fatal("ReadBundle of corrupt data.json must error")
	}
}

// TestMarkFilesDanglingSymlink checks the documented dangling-symlink-as-absent
// behavior: a declared output that is a symlink to a nonexistent target stats as
// absent (os.Stat follows the link), so MarkFiles keeps the path unchanged rather
// than aborting the bundle.
func TestMarkFilesDanglingSymlink(t *testing.T) {
	dir := t.TempDir()

	dangling := filepath.Join(dir, "dangling")
	if err := os.Symlink(filepath.Join(dir, "nonexistent-target"), dangling); err != nil {
		t.Fatal(err)
	}

	params := []ir.Param{{Name: "f", BaseType: "file", IsFile: true}}
	marked, err := shim.MarkFiles(dir, map[string]any{"f": dangling}, params, fileTable(), nil)
	if err != nil {
		t.Fatalf("MarkFiles must treat a dangling symlink as absent, not error: %v", err)
	}
	if marked["f"] != dangling {
		t.Errorf("dangling output f = %v, want the unchanged path %q", marked["f"], dangling)
	}
}
