package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/types"
)

// keyOrderManifest declares In(z, a), Outer(inner In, b) and a file-bearing
// FOut(q In, f file, b), whose declared and sorted orders differ, with PIPE
// returning one plain and one file-bearing struct.
func keyOrderManifest(t *testing.T, dir string) string {
	t.Helper()

	man := types.Manifest{
		Structs: map[string]*ir.StructType{
			"In": {Name: "In", Fields: []ir.Param{{Name: "z", BaseType: "int"}, {Name: "a", BaseType: "int"}}},
			"Outer": {Name: "Outer", Fields: []ir.Param{
				{Name: "inner", BaseType: "In"}, {Name: "b", BaseType: "int"},
			}},
			"FOut": {Name: "FOut", Fields: []ir.Param{
				{Name: "q", BaseType: "In"}, {Name: "f", BaseType: "file", IsFile: true}, {Name: "b", BaseType: "int"},
			}},
		},
		Callables: map[string]types.Callable{
			"PIPE": {Out: []ir.Param{
				{Name: "st", BaseType: "Outer"}, {Name: "fs", BaseType: "FOut", IsFile: true},
			}},
		},
	}

	return writeTestManifest(t, filepath.Join(dir, "types.json"), man)
}

func compactData(t *testing.T, path string) string {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatalf("compact %s: %v", path, err)
	}

	return buf.String()
}

// TestRunPublishLayoutKeepsKeyOrder checks pipeline_outs.json keeps a plain
// struct output in its producer's order (kitchen_sink's stats, #366), while a
// file-bearing struct takes sorted keys, as mrp's moveOutDir writes it, with
// its plain member as produced.
func TestRunPublishLayoutKeepsKeyOrder(t *testing.T) {
	dir := t.TempDir()
	sidecar := writeTestFile(t, filepath.Join(dir, "data.json"),
		`{"fs":{"q":{"a":2,"z":1},"f":"`+marker("L0000")+`","b":3},"st":{"inner":{"a":2,"z":1},"b":3}}`)
	outDir := filepath.Join(dir, "outs")

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}

	argv := []string{
		"publish-layout", "-types", keyOrderManifest(t, dir), "-callable", "PIPE",
		"-sidecar", sidecar, "-dir", outDir,
	}
	if err := run(t.Context(), argv); err != nil {
		t.Fatalf("run publish-layout: %v", err)
	}

	want := `{"fs":{"b":3,"f":"fs/f","q":{"a":2,"z":1}},"st":{"inner":{"a":2,"z":1},"b":3}}`
	if got := compactData(t, filepath.Join(outDir, "pipeline_outs.json")); got != want {
		t.Errorf("pipeline_outs.json = %s, want %s", got, want)
	}
}
