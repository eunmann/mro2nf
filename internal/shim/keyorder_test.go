package shim_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/eunmann/mro2nf/internal/types"
	"github.com/martian-lang/martian/martian/syntax"
)

func compactJSON(t *testing.T, raw []byte) string {
	t.Helper()

	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatalf("compact %s: %v", raw, err)
	}

	return buf.String()
}

func readData(t *testing.T, dir string) string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(dir, "data.json"))
	if err != nil {
		t.Fatal(err)
	}

	return compactJSON(t, raw)
}

// TestReadBundleKeepsKeyOrder checks a bundle reads back with every nested
// object in the order its writer gave it, as mrp hands a consumer its
// producer's bytes, while markers still resolve and escapes still restore.
func TestReadBundleKeepsKeyOrder(t *testing.T) {
	dir := t.TempDir()
	data := `{"o":{"inner":{"a":2},"b":3},"m":{"y":"@mre:str:@mre:x","x":["@mre:file:f/L0000/r.txt"]}}`

	if err := os.WriteFile(filepath.Join(dir, "data.json"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	raw, err := shim.Refs{}.ReadBundle(dir)
	if err != nil {
		t.Fatalf("ReadBundle: %v", err)
	}

	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}

	want := `{"o":{"inner":{"a":2},"b":3},"m":{"y":"@mre:x","x":["` + filepath.Join(abs, "f/L0000/r.txt") + `"]}}`
	if got := compactJSON(t, raw); got != want {
		t.Errorf("ReadBundle = %s, want %s", got, want)
	}
}

// TestWriteItemKeepsKeyOrder checks a value read from one bundle is written
// to the next with its nested objects in the same order, its file leaves
// staged, and its top level sorted as mrp writes an _args or _outs.
func TestWriteItemKeepsKeyOrder(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "ref.txt")

	if err := os.WriteFile(src, []byte("r"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := types.DecodeJSON([]byte(`{"z":1,"ref":"` + src + `","a":{"y":1,"x":2}}`))
	if err != nil {
		t.Fatal(err)
	}

	_, _, ast, err := syntax.ParseSourceBytes([]byte("struct Cfg(\n    int  z,\n    file ref,\n    map  a,\n)\n"), "keyorder.mro", nil, false)
	if err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "out")

	if _, err := shim.WriteItem(out, map[string]any{"cfg": cfg, "b": 1}, fields(t, "cfg Cfg"), types.NewTable(&ast.TypeTable), shim.Opts{}, shim.Staged{}); err != nil {
		t.Fatalf("WriteItem: %v", err)
	}

	want := `{"b":1,"cfg":{"z":1,"ref":"@mre:file:f/L0000/ref.txt","a":{"y":1,"x":2}}}`
	if got := readData(t, out); got != want {
		t.Errorf("data.json = %s, want %s", got, want)
	}
}
