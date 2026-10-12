package cg

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/martian-lang/martian/martian/core"
)

func TestKeepNegativeZero(t *testing.T) {
	for in, want := range map[string]string{
		"x = [1e+21, -0],":             "x = [1e+21, -0.0],",
		"-0":                           "-0.0",
		`{"a":-0,"b":[-0]}`:            `{"a":-0.0,"b":[-0.0]}`,
		"x = -0.5,":                    "x = -0.5,",
		"x = -0e5,":                    "x = -0e5,",
		"x = 10,":                      "x = 10,",
		`x = "-0",`:                    `x = "-0",`,
		`x = "a\"-0",`:                 `x = "a\"-0",`,
		"# an \"odd quote -0\nx = -0,": "# an \"odd quote -0\nx = -0.0,",
	} {
		if got := keepNegativeZero(in); got != want {
			t.Errorf("keepNegativeZero(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestRebuildKeepsNegativeZero: a run that overrides nothing compiles the
// float literal -0.0 to -0, as mrp compiles the entry file, through the
// formatted source and the rebuilt call (gen-diff seed 22: mrp passes
// i1 = [1e+21, -0]).
func TestRebuildKeepsNegativeZero(t *testing.T) {
	dir := t.TempDir()
	mro := filepath.Join(dir, "pipeline.mro")
	src := "stage S(\n    in  float[] x,\n    out int     y,\n    src py      \"s\",\n)\n\npipeline P(\n    in  float[] xs,\n    out int     y,\n)\n{\n    call S(\n        x = self.xs,\n    )\n\n    return (\n        y = S.y,\n    )\n}\n\ncall P(\n    xs = [1e21, -0.0],\n)\n"

	if err := os.WriteFile(mro, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := ReadSource(mro, []string{dir})
	if err != nil {
		t.Fatal(err)
	}

	text, err := s.Rebuild(&core.InvocationData{Call: s.Invocation.Call, Args: s.Invocation.Args}, []string{dir})
	if err != nil {
		t.Fatal(err)
	}

	g, err := Compile([]byte(text), s.Name, []string{dir})
	if err != nil {
		t.Fatal(err)
	}

	st, err := g.Stage("P.S")
	if err != nil {
		t.Fatal(err)
	}

	raw, err := st.ResolvedInputs()["x"].Exp.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}

	if string(raw) != "[1e+21,-0]" {
		t.Errorf("x = %s, mrp's is [1e+21,-0]", raw)
	}
}
