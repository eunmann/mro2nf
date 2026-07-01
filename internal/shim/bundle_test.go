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
