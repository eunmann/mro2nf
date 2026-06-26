package frontend_test

import (
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/eunmann/martian-nextflow/internal/frontend"
	"github.com/eunmann/martian-nextflow/internal/ir"
)

const splitTestMRO = "../../testdata/split_test/pipeline.mro"

func loadProgram(t *testing.T) *ir.Program {
	t.Helper()

	ast, err := frontend.Parse(splitTestMRO, []string{"../../testdata/split_test"}, false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	prog, err := frontend.Lower(ast)
	if err != nil {
		t.Fatalf("lower: %v", err)
	}

	return prog
}

func TestLowerStages(t *testing.T) {
	prog := loadProgram(t)

	if len(prog.Stages) != 2 {
		t.Fatalf("stages = %d, want 2", len(prog.Stages))
	}

	ss := prog.Stages["SUM_SQUARES"]
	if ss == nil {
		t.Fatal("missing stage SUM_SQUARES")
	}

	t.Run("split flags and adapter", func(t *testing.T) {
		if !ss.Split {
			t.Error("SUM_SQUARES should be a split stage")
		}
		if ss.Lang != ir.LangPy {
			t.Errorf("lang = %q, want py", ss.Lang)
		}
		if ss.SrcPath == "" {
			t.Error("src path should be resolved")
		}
	})

	t.Run("typed params", func(t *testing.T) {
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
	})

	t.Run("resources", func(t *testing.T) {
		if ss.Resources.MemGB != 2 {
			t.Errorf("SUM_SQUARES mem_gb = %v, want 2", ss.Resources.MemGB)
		}
		rep := prog.Stages["REPORT"]
		if rep == nil {
			t.Fatal("missing stage REPORT")
		}
		if rep.Split {
			t.Error("REPORT should not be a split stage")
		}
		if rep.Resources.Threads != 0.5 {
			t.Errorf("REPORT threads = %v, want 0.5", rep.Resources.Threads)
		}
	})
}

func TestLowerPipelineWiring(t *testing.T) {
	prog := loadProgram(t)

	pl := prog.Pipelines["SUM_SQUARE_PIPELINE"]
	if pl == nil {
		t.Fatal("missing pipeline SUM_SQUARE_PIPELINE")
	}
	if len(pl.Calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(pl.Calls))
	}

	report := findCall(pl.Calls, "REPORT")
	if report == nil {
		t.Fatal("missing REPORT call")
	}

	sum := findBinding(report.Bindings, "sum")
	if sum == nil || sum.Value.Ref == nil {
		t.Fatal("REPORT.sum should be a reference binding")
	}

	want := &ir.Ref{Kind: "call", ID: "SUM_SQUARES", Output: "sum"}
	if diff := cmp.Diff(want, sum.Value.Ref); diff != "" {
		t.Errorf("REPORT.sum ref mismatch (-want +got):\n%s", diff)
	}

	ret := findBinding(pl.Returns, "sum")
	if ret == nil || ret.Value.Ref == nil || ret.Value.Ref.ID != "SUM_SQUARES" || ret.Value.Ref.Output != "sum" {
		t.Errorf("return sum should reference SUM_SQUARES.sum, got %+v", ret)
	}
}

func TestLowerEntry(t *testing.T) {
	prog := loadProgram(t)

	if prog.Entry == nil {
		t.Fatal("missing entry call")
	}
	if prog.Entry.Callable != "SUM_SQUARE_PIPELINE" {
		t.Errorf("entry callable = %q, want SUM_SQUARE_PIPELINE", prog.Entry.Callable)
	}

	values := findBinding(prog.Entry.Bindings, "values")
	if values == nil || values.Value.Array == nil {
		t.Fatal("entry values should be an array binding")
	}

	got := make([]int, 0, len(values.Value.Array))
	for _, el := range values.Value.Array {
		var n int
		if err := json.Unmarshal(el.Literal, &n); err != nil {
			t.Fatalf("unmarshal element: %v", err)
		}
		got = append(got, n)
	}

	if diff := cmp.Diff([]int{1, 2, 3}, got); diff != "" {
		t.Errorf("entry values mismatch (-want +got):\n%s", diff)
	}
}

func TestLowerStructs(t *testing.T) {
	ast, err := frontend.Parse("../../testdata/nested_struct/pipeline.mro", []string{"../../testdata/nested_struct"}, false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	prog, err := frontend.Lower(ast)
	if err != nil {
		t.Fatalf("lower: %v", err)
	}

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
	prog := loadProgram(t)

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

func findCall(calls []ir.Call, name string) *ir.Call {
	for i := range calls {
		if calls[i].Name == name {
			return &calls[i]
		}
	}

	return nil
}

func findBinding(bindings []ir.Binding, param string) *ir.Binding {
	for i := range bindings {
		if bindings[i].Param == param {
			return &bindings[i]
		}
	}

	return nil
}
