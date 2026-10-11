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

// TestMarkFilesPreservesExtension guards a real CellRanger failure: Martian
// typed-file readers reconstruct a leaf's on-disk path from its filetype
// extension (the Rust equivalent of Path::with_extension("bincode")). A staged
// leaf therefore must keep the source file's extension, or the reader looks for
// `<leaf>.bincode` next to a bare `<leaf>` and fails with NotFound. The transport
// leaf stays ordinally named (collision-free) but carries the original extension.
func TestMarkFilesPreservesExtension(t *testing.T) {
	dir := t.TempDir()

	srcDir := filepath.Join(dir, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}

	src := filepath.Join(srcDir, "filtered_bcs_groups.bincode")
	if err := os.WriteFile(src, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}

	params := []ir.Param{{Name: "f", BaseType: "bincode", IsFile: true}}
	bundle := filepath.Join(dir, "bundle")
	if err := shim.WriteBundle(bundle, map[string]any{"f": src}, params, fileTable(), nil); err != nil {
		t.Fatalf("WriteBundle: %v", err)
	}

	// The leaf keeps the .bincode extension so with_extension("bincode") resolves.
	if _, err := os.Stat(filepath.Join(bundle, "f", "L0000.bincode")); err != nil {
		t.Errorf("leaf should be staged as f/L0000.bincode (extension preserved): %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(bundle, "data.json"))
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(raw), "@mre:file:f/L0000.bincode") {
		t.Errorf("marker should carry the .bincode extension, got: %s", raw)
	}
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

	// The file was staged into the bundle under a flat ordinal leaf name (the
	// original basename is dropped, but its extension is preserved) and the payload
	// now holds its marker.
	raw, err := os.ReadFile(filepath.Join(bundle, "data.json"))
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(raw), "@mre:file:f/L0000.txt") {
		t.Errorf("data.json should contain a flat ordinal file marker, got: %s", raw)
	}

	if _, err := os.Stat(filepath.Join(bundle, "f", "L0000.txt")); err != nil {
		t.Errorf("leaf should be staged as f/L0000.txt: %v", err)
	}

	// Read it back; the marker resolves to an absolute path that exists.
	resolved, err := shim.ReadBundle(bundle)
	if err != nil {
		t.Fatalf("ReadBundle: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(resolved, &got); err != nil {
		t.Fatal(err)
	}

	path, ok := got["f"].(string)
	if !ok || !filepath.IsAbs(path) {
		t.Fatalf("resolved f = %v, want an absolute path", got["f"])
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

// TestMarkerRoundTrip pins the marker encode/decode contract both ways.
func TestMarkerRoundTrip(t *testing.T) {
	cases := []struct {
		rel   string
		isDir bool
	}{
		{"f/L0000", false},
		{"f/L0001", true},
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
		"@mre:file:f/L0000.txt",
		"@mre:dir:f/L0000.txt",
		"@mre:str:@mre:file:f/L0000.txt",
		"@mre:",
		"@mre:file:../elsewhere",
		"plain",
	}
	payload := map[string]any{
		"made":  src,
		"fake":  "@mre:file:f/L0000.txt",
		"s":     "@mre:file:f/L0000.txt",
		"arr":   strs,
		"m":     map[string]any{"k": "@mre:dir:f/L0000.txt", "nested": strs},
		"extra": "@mre:file:f/L0000.txt",
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
		"made":  filepath.Join(abs, "f", "L0000.txt"),
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
// marker only as f/L<ordinal>[.ext] inside the bundle and escapes every other
// reserved string, so a payload string carrying the reserved prefix in any
// other shape (a rel leaving the bundle, an absolute rel, an unknown kind) is a
// corrupt or forged bundle and fails the read instead of resolving to a path.
func TestReadBundleRejectsMalformedMarkers(t *testing.T) {
	bad := []string{
		"@mre:file:../elsewhere",
		"@mre:dir:/abs/path",
		"@mre:file:f/../../elsewhere",
		"@mre:file:f/L12.txt",
		"@mre:file:g/L0000",
		"@mre:file:f/L0000/sub",
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
		"@mre:file:f/L0000":         "f/L0000",
		"@mre:file:f/L12345.tar.gz": "f/L12345.tar.gz",
		"@mre:dir:f/L0001":          "f/L0001",
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
