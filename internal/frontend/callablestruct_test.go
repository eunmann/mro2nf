package frontend_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/eunmann/mro2nf/internal/frontend"
	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/types"
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

	// Cross the seam into the type-walk, the behavior #173 is actually about: the
	// pipeline output `made` is typed by the callable, so it must publish as the
	// bare name `made` (a directory) and its inner `data` file leaf must be
	// rewritten — not published as `made.MAKE` with the file lost.
	top := prog.Pipelines["TOP"]
	if top == nil {
		t.Fatal("pipeline TOP not lowered")
	}

	var made ir.Param
	for _, o := range top.Out {
		if o.Name == "made" {
			made = o
		}
	}
	if made.Name == "" {
		t.Fatalf("TOP.made not found in out params: %+v", top.Out)
	}

	tbl := types.NewTable(prog.Structs)

	// Without the registered struct, OutFilename falls to the `name.type` arm and
	// publishes `made.MAKE`.
	if got := types.OutFilename(made, tbl.IsStruct); got != "made" {
		t.Errorf("OutFilename(made) = %q, want %q (callable struct must be recognized)", got, "made")
	}

	// The walk must descend the struct and rewrite the inner `data` file leaf.
	var rewritten []string
	out, err := tbl.Apply([]ir.Param{made}, map[string]any{
		"made": map[string]any{"data": "in/f", "count": float64(5)},
	}, func(path string) (string, error) {
		rewritten = append(rewritten, path)
		return "staged/" + path, nil
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(rewritten) != 1 || rewritten[0] != "in/f" {
		t.Errorf("walk rewrote %v, want the single leaf [in/f] (struct must descend into `data`)", rewritten)
	}
	if inner, _ := out["made"].(map[string]any); inner["data"] != "staged/in/f" {
		t.Errorf("made.data = %v, want the rewritten path (leaf not staged)", out["made"])
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	return out
}
