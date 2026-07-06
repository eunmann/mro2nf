package shim_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/eunmann/mro2nf/internal/types"
)

func fileTable() *types.Table {
	return types.NewTable(map[string]*ir.StructType{
		"Cfg": {Name: "Cfg", Fields: []ir.Param{{Name: "ref", BaseType: "file", IsFile: true}}},
	})
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
	if err := shim.WriteBundle(bundle, payload, params, fileTable()); err != nil {
		t.Fatalf("WriteBundle: %v", err)
	}

	// The file was staged into the bundle under a flat ordinal leaf name (no
	// original basename in transport) and the payload now holds its marker.
	raw, err := os.ReadFile(filepath.Join(bundle, "data.json"))
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(raw), "@mre:file:f/L0000") {
		t.Errorf("data.json should contain a flat ordinal file marker, got: %s", raw)
	}

	if _, err := os.Stat(filepath.Join(bundle, "f", "L0000")); err != nil {
		t.Errorf("leaf should be staged as f/L0000: %v", err)
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

	marked, err := shim.MarkFiles(dir, payload, params, fileTable())
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
	if err := shim.WriteBundle(bundle, payload, params, fileTable()); err != nil {
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

	marked, err := shim.MarkFiles(dir, payload, params, fileTable())
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

// TestCopyTreeSymlinkCycle guards against unbounded recursion: a directory output
// containing a symlink back to one of its ancestors would otherwise recurse until
// ENAMETOOLONG, duplicating the tree on the way. CopyTree must instead stop with a
// cycle error.
func TestCopyTreeSymlinkCycle(t *testing.T) {
	dir := t.TempDir()

	root := filepath.Join(dir, "out")
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	// out/sub/loop -> out : recursing into loop re-enters out, an ancestor.
	if err := os.Symlink(root, filepath.Join(root, "sub", "loop")); err != nil {
		t.Fatal(err)
	}

	err := shim.CopyTree(root, filepath.Join(dir, "copy"))
	if err == nil {
		t.Fatal("CopyTree over a symlink cycle must error, not recurse unbounded")
	}
	if !strings.Contains(err.Error(), "symlink cycle") {
		t.Errorf("CopyTree cycle error = %v, want a symlink-cycle error", err)
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
	marked, err := shim.MarkFiles(dir, map[string]any{"f": dangling}, params, fileTable())
	if err != nil {
		t.Fatalf("MarkFiles must treat a dangling symlink as absent, not error: %v", err)
	}
	if marked["f"] != dangling {
		t.Errorf("dangling output f = %v, want the unchanged path %q", marked["f"], dangling)
	}
}
