package frontend_test

import (
	"testing"

	"github.com/eunmann/mro2nf/internal/frontend"
	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/martian-lang/martian/martian/syntax"
)

const splitTestMRO = "../../testdata/split_test/pipeline.mro"

// lower compiles the MRO file at path as mrp does, resolving @include
// against mroPaths, and lowers it.
func lower(t *testing.T, path string, mroPaths []string) *ir.Program {
	t.Helper()

	_, _, ast, err := syntax.Compile(path, mroPaths, false)
	if err != nil {
		t.Fatalf("compile %s: %v", path, err)
	}

	return frontend.Lower(ast)
}

func TestLowerStages(t *testing.T) {
	prog := lower(t, splitTestMRO, []string{"../../testdata/split_test"})

	if len(prog.Stages) != 2 {
		t.Fatalf("stages = %d, want 2", len(prog.Stages))
	}

	ss := prog.Stages["SUM_SQUARES"]
	if ss == nil {
		t.Fatal("missing stage SUM_SQUARES")
	}

	if got := paramType(ss.In, "values"); got != "float[]" {
		t.Errorf("in values type = %q, want float[]", got)
	}

	if got := paramType(ss.Out, "sum"); got != "float" {
		t.Errorf("out sum type = %q, want float", got)
	}

	if got := paramType(ss.ChunkIn, "value"); got != "float" {
		t.Errorf("chunk-in value type = %q, want float", got)
	}

	if got := paramType(ss.ChunkOut, "square"); got != "float" {
		t.Errorf("chunk-out square type = %q, want float", got)
	}

	if p := prog.Pipelines["SUM_SQUARE_PIPELINE"]; p == nil || paramType(p.Out, "sum") != "float" {
		t.Errorf("pipeline SUM_SQUARE_PIPELINE = %+v, want its float out sum", p)
	}
}

func TestLowerStructs(t *testing.T) {
	prog := lower(t, "../../testdata/nested_struct/pipeline.mro", []string{"../../testdata/nested_struct"})

	outer := prog.Structs["Outer"]
	if outer == nil {
		t.Fatal("missing struct Outer")
	}

	if len(outer.Fields) != 2 {
		t.Fatalf("Outer fields = %d, want 2", len(outer.Fields))
	}

	inner := findParam(outer.Fields, "inner")
	if inner == nil {
		t.Fatal("missing Outer.inner field")
	}

	if inner.BaseType != "Inner" {
		t.Errorf("Outer.inner BaseType = %q, want Inner", inner.BaseType)
	}

	if prog.Structs["Inner"] == nil {
		t.Error("missing struct Inner")
	}
}

func TestLowerParamDims(t *testing.T) {
	prog := lower(t, splitTestMRO, []string{"../../testdata/split_test"})

	values := findParam(prog.Stages["SUM_SQUARES"].In, "values")
	if values == nil {
		t.Fatal("missing SUM_SQUARES.values")
	}

	if values.BaseType != "float" || values.ArrayDim != 1 || values.MapDim != 0 {
		t.Errorf("values dims = {base:%q arr:%d map:%d}, want {float 1 0}",
			values.BaseType, values.ArrayDim, values.MapDim)
	}
}

func findParam(params []ir.Param, name string) *ir.Param {
	for i := range params {
		if params[i].Name == name {
			return &params[i]
		}
	}

	return nil
}

func paramType(params []ir.Param, name string) string {
	for _, p := range params {
		if p.Name == name {
			return p.Type
		}
	}

	return ""
}
