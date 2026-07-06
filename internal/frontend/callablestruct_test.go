package frontend_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/eunmann/mro2nf/internal/frontend"
)

// TestLowerRegistersCallableStructs guards #173: a param typed by a callable name
// (`out MAKE made`, MAKE a stage) is a legal Martian struct whose fields are the
// callable's outputs. lowerStructs only saw explicit `struct` decls, so such a
// param lowered to an opaque leaf and the file paths inside it were never
// rewritten/staged/published. Lower must register the callable-derived struct.
func TestLowerRegistersCallableStructs(t *testing.T) {
	const src = `
stage MAKE(
    out file  data,
    out int   count,
    src py     "make.py",
)

pipeline TOP(
    out MAKE made,
)
{
    call MAKE()
    return (
        made = MAKE,
    )
}
`
	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.mro")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	ast, err := frontend.Parse(path, []string{dir}, false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	prog, err := frontend.Lower(ast)
	if err != nil {
		t.Fatalf("lower: %v", err)
	}

	st, ok := prog.Structs["MAKE"]
	if !ok {
		t.Fatalf("callable-derived struct MAKE not registered; Structs = %v", keys(prog.Structs))
	}

	// The struct carries MAKE's outputs, so the file-leaf walk can find `data`.
	var dataField, hasFile bool
	for _, f := range st.Fields {
		if f.Name == "data" {
			dataField = true
			hasFile = f.IsFile
		}
	}
	if !dataField {
		t.Errorf("MAKE struct missing the `data` output field; fields = %+v", st.Fields)
	}
	if !hasFile {
		t.Errorf("MAKE.data must be a file leaf so it is staged/published")
	}

	// The pipeline output `made` is typed by the callable, so its file leaves
	// resolve through the registered struct rather than being lost.
	top := prog.Pipelines["TOP"]
	if top == nil {
		t.Fatal("pipeline TOP not lowered")
	}
	var madeTyped bool
	for _, o := range top.Out {
		if o.Name == "made" && o.BaseType == "MAKE" {
			madeTyped = true
		}
	}
	if !madeTyped {
		t.Errorf("TOP.made should be typed MAKE; out = %+v", top.Out)
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	return out
}
