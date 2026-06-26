package frontend

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/martian-lang/martian/martian/syntax"

	"github.com/eunmann/martian-nextflow/internal/apperror"
	"github.com/eunmann/martian-nextflow/internal/ir"
)

// Lower converts a type-checked Martian AST into the transpiler IR.
func Lower(ast *syntax.Ast) (*ir.Program, error) {
	prog := &ir.Program{
		Stages:    make(map[string]*ir.Stage, len(ast.Stages)),
		Pipelines: make(map[string]*ir.Pipeline, len(ast.Pipelines)),
	}

	for _, s := range ast.Stages {
		prog.Stages[s.Id] = lowerStage(s)
	}

	for _, p := range ast.Pipelines {
		lp, err := lowerPipeline(p)
		if err != nil {
			return nil, err
		}

		prog.Pipelines[p.Id] = lp
	}

	if ast.Call != nil {
		entry, err := lowerEntry(ast.Call)
		if err != nil {
			return nil, err
		}

		prog.Entry = entry
	}

	return prog, nil
}

func lowerStage(s *syntax.Stage) *ir.Stage {
	st := &ir.Stage{
		Name:     s.Id,
		In:       lowerInParams(s.InParams),
		Out:      lowerOutParams(s.OutParams),
		Split:    s.Split,
		ChunkIn:  lowerInParams(s.ChunkIns),
		ChunkOut: lowerOutParams(s.ChunkOuts),
	}

	if s.Src != nil {
		st.Lang = ir.Lang(s.Src.Lang)
		st.SrcPath = s.Src.Path
		st.SrcArgs = s.Src.Args
	}

	if r := s.Resources; r != nil {
		st.Resources = ir.Resources{
			MemGB:   float64(r.MemGB),
			VMemGB:  float64(r.VMemGB),
			Threads: float64(r.Threads),
			Special: r.Special,
		}
	}

	return st
}

func lowerPipeline(p *syntax.Pipeline) (*ir.Pipeline, error) {
	lp := &ir.Pipeline{
		Name: p.Id,
		In:   lowerInParams(p.InParams),
		Out:  lowerOutParams(p.OutParams),
	}

	for _, c := range p.Calls {
		lc, err := lowerCall(c)
		if err != nil {
			return nil, fmt.Errorf("pipeline %s: %w", p.Id, err)
		}

		lp.Calls = append(lp.Calls, lc)
	}

	if p.Ret != nil {
		ret, err := lowerBindings(p.Ret.Bindings)
		if err != nil {
			return nil, fmt.Errorf("pipeline %s return: %w", p.Id, err)
		}

		lp.Returns = ret
	}

	return lp, nil
}

func lowerCall(c *syntax.CallStm) (ir.Call, error) {
	lc := ir.Call{Name: c.Id, Callable: c.DecId, Mapped: c.Mapping != nil}

	if m := c.Modifiers; m != nil {
		lc.Local = m.Local
		lc.Preflight = m.Preflight
		lc.Volatile = m.Volatile
		lc.Disabled = disabledRef(m)
	}

	b, err := lowerBindings(c.Bindings)
	if err != nil {
		return lc, fmt.Errorf("call %s: %w", c.Id, err)
	}

	lc.Bindings = b

	return lc, nil
}

func lowerEntry(c *syntax.CallStm) (*ir.EntryCall, error) {
	b, err := lowerBindings(c.Bindings)
	if err != nil {
		return nil, fmt.Errorf("entry call %s: %w", c.DecId, err)
	}

	return &ir.EntryCall{Callable: c.DecId, Bindings: b}, nil
}

func lowerBindings(bs *syntax.BindStms) ([]ir.Binding, error) {
	if bs == nil {
		return nil, nil
	}

	out := make([]ir.Binding, 0, len(bs.List))

	for _, b := range bs.List {
		lb, err := lowerBinding(b)
		if err != nil {
			return nil, err
		}

		out = append(out, lb)
	}

	return out, nil
}

func lowerBinding(bs *syntax.BindStm) (ir.Binding, error) {
	b := ir.Binding{Param: bs.Id}

	exp := bs.Exp
	if se, ok := exp.(*syntax.SplitExp); ok {
		b.Split = true
		exp = se.Value
	}

	if r, ok := exp.(*syntax.RefExp); ok {
		b.Ref = refFrom(r)

		return b, nil
	}

	v, err := litToGo(exp)
	if err != nil {
		return b, fmt.Errorf("binding %q: %w", bs.Id, err)
	}

	raw, err := json.Marshal(v)
	if err != nil {
		return b, fmt.Errorf("binding %q: marshal literal: %w", bs.Id, err)
	}

	b.Literal = raw

	return b, nil
}

func refFrom(r *syntax.RefExp) *ir.Ref {
	return &ir.Ref{Kind: string(r.Kind), ID: r.Id, Output: r.OutputId}
}

// disabledRef extracts the `disabled = <ref>` call modifier, if present.
func disabledRef(m *syntax.Modifiers) *ir.Ref {
	if m.Bindings == nil {
		return nil
	}

	d, ok := m.Bindings.Table["disabled"]
	if !ok {
		return nil
	}

	r, ok := d.Exp.(*syntax.RefExp)
	if !ok {
		return nil
	}

	return refFrom(r)
}

// litToGo converts a literal value expression into a Go value ready for JSON
// encoding. It returns an UnsupportedError if it encounters a reference, which
// is only valid in a binding position, not nested inside a constant.
//
// Recursion depth is bounded by the nesting of the parsed MRO literal, which the
// Martian compiler has already produced as a finite AST.
func litToGo(e syntax.Exp) (any, error) {
	switch v := e.(type) {
	case *syntax.IntExp:
		return v.Value, nil
	case *syntax.FloatExp:
		return v.Value, nil
	case *syntax.StringExp:
		return v.Value, nil
	case *syntax.BoolExp:
		return v.Value, nil
	case *syntax.NullExp:
		return nil, nil
	case *syntax.ArrayExp:
		arr := make([]any, 0, len(v.Value))

		for _, el := range v.Value {
			g, err := litToGo(el)
			if err != nil {
				return nil, err
			}

			arr = append(arr, g)
		}

		return arr, nil
	case *syntax.MapExp:
		m := make(map[string]any, len(v.Value))

		for k, el := range v.Value {
			g, err := litToGo(el)
			if err != nil {
				return nil, err
			}

			m[k] = g
		}

		return m, nil
	default:
		return nil, &apperror.UnsupportedError{
			Construct: "non-literal value",
			Detail:    fmt.Sprintf("expression %T cannot appear as a constant", e),
		}
	}
}

func lowerInParams(p *syntax.InParams) []ir.Param {
	if p == nil {
		return nil
	}

	out := make([]ir.Param, 0, len(p.List))
	for _, ip := range p.List {
		out = append(out, paramFrom(ip))
	}

	return out
}

func lowerOutParams(p *syntax.OutParams) []ir.Param {
	if p == nil {
		return nil
	}

	out := make([]ir.Param, 0, len(p.List))
	for _, op := range p.List {
		out = append(out, paramFrom(op))
	}

	return out
}

func paramFrom(p syntax.Param) ir.Param {
	t := p.GetTname()

	return ir.Param{
		Name:     p.GetId(),
		Type:     renderType(t),
		ArrayDim: int(t.ArrayDim),
		IsFile:   p.IsFile() != syntax.KindIsNotFile,
		OutName:  p.GetOutName(),
	}
}

// renderType reconstructs the MRO surface type string from a TypeId, including
// array dimensions and typed-map wrapping.
func renderType(t syntax.TypeId) string {
	base := t.Tname

	if t.MapDim > 0 {
		inner := base + strings.Repeat("[]", int(t.MapDim-1))
		base = "map<" + inner + ">"
	}

	return base + strings.Repeat("[]", int(t.ArrayDim))
}
