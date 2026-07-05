package frontend

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/eunmann/mro2nf/internal/apperror"
	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/martian-lang/martian/martian/syntax"
)

// Lower converts a type-checked Martian AST into the transpiler IR.
func Lower(ast *syntax.Ast) (*ir.Program, error) {
	prog := &ir.Program{
		Stages:    make(map[string]*ir.Stage, len(ast.Stages)),
		Pipelines: make(map[string]*ir.Pipeline, len(ast.Pipelines)),
		Structs:   lowerStructs(ast),
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

	if c.Mapping != nil {
		lc.MapMode = c.Mapping.CallMode().String()
	}

	if m := c.Modifiers; m != nil {
		lc.Local = m.Local
		lc.Preflight = m.Preflight
		lc.Volatile = m.Volatile

		disabled, err := disabledRef(m)
		if err != nil {
			return lc, fmt.Errorf("call %s: %w", c.Id, err)
		}

		lc.Disabled = disabled
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

	v, err := lowerExp(exp)
	if err != nil {
		return b, fmt.Errorf("binding %q: %w", bs.Id, err)
	}

	b.Value = v

	return b, nil
}

func refFrom(r *syntax.RefExp) *ir.Ref {
	return &ir.Ref{Kind: string(r.Kind), ID: r.Id, Output: r.OutputId}
}

// disabledRef extracts the `disabled = <ref>` call modifier, if present. Any
// other expression shape is unsupported and fails loudly: silently dropping it
// would lower the call as "always enabled", inverting e.g. `disabled = true`.
func disabledRef(m *syntax.Modifiers) (*ir.Ref, error) {
	if m.Bindings == nil {
		return nil, nil
	}

	d, ok := m.Bindings.Table["disabled"]
	if !ok {
		return nil, nil
	}

	r, ok := d.Exp.(*syntax.RefExp)
	if !ok {
		return nil, &apperror.UnsupportedError{
			Construct: "disabled modifier",
			Detail:    fmt.Sprintf("%T expression (only a reference is supported)", d.Exp),
		}
	}

	return refFrom(r), nil
}

// lowerExp lowers a value expression into an ir.Value tree, preserving any
// references nested inside array/map literals (e.g. a fan-in `[A.out, B.out]`).
// Recursion depth is bounded by the finite parsed MRO AST.
func lowerExp(e syntax.Exp) (ir.Value, error) {
	switch v := e.(type) {
	case *syntax.RefExp:
		return ir.Value{Ref: refFrom(v)}, nil
	case *syntax.ArrayExp:
		arr := make([]ir.Value, 0, len(v.Value))

		for _, el := range v.Value {
			lv, err := lowerExp(el)
			if err != nil {
				return ir.Value{}, err
			}

			arr = append(arr, lv)
		}

		return ir.Value{Array: arr}, nil
	case *syntax.MapExp:
		obj := make(map[string]ir.Value, len(v.Value))

		for k, el := range v.Value {
			lv, err := lowerExp(el)
			if err != nil {
				return ir.Value{}, err
			}

			obj[k] = lv
		}

		return ir.Value{Object: obj}, nil
	default:
		raw, err := litLeaf(e)
		if err != nil {
			return ir.Value{}, err
		}

		return ir.Value{Literal: raw}, nil
	}
}

// litLeaf marshals a scalar literal leaf (int/float/string/bool/null).
func litLeaf(e syntax.Exp) (json.RawMessage, error) {
	var v any

	switch x := e.(type) {
	case *syntax.IntExp:
		v = x.Value
	case *syntax.FloatExp:
		v = x.Value
	case *syntax.StringExp:
		v = x.Value
	case *syntax.BoolExp:
		v = x.Value
	case *syntax.NullExp:
		v = nil
	default:
		return nil, &apperror.UnsupportedError{
			Construct: "expression",
			Detail:    fmt.Sprintf("%T cannot be lowered", e),
		}
	}

	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal literal: %w", err)
	}

	return raw, nil
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
	return paramFromParts(p.GetId(), p.GetTname(), isFileKind(p.IsFile()), p.GetOutName())
}

// isFileKind reports whether a parameter's file kind denotes a real file or
// directory to stage. KindMayContainPaths (e.g. a plain string) is a VDR
// heuristic, not a file, so it is excluded.
func isFileKind(fk syntax.FileKind) bool {
	return fk == syntax.KindIsFile || fk == syntax.KindIsDirectory
}

// paramFromParts builds an ir.Param from the common parts shared by stage/
// pipeline params and struct members.
func paramFromParts(id string, t syntax.TypeId, isFile bool, outName string) ir.Param {
	return ir.Param{
		Name:     id,
		Type:     renderType(t),
		BaseType: t.Tname,
		ArrayDim: int(t.ArrayDim),
		MapDim:   int(t.MapDim),
		IsFile:   isFile,
		OutName:  outName,
	}
}

// lowerStructs extracts explicit `struct` type declarations so the file-leaf
// walk can expand nested struct-typed values.
func lowerStructs(ast *syntax.Ast) map[string]*ir.StructType {
	structs := make(map[string]*ir.StructType, len(ast.StructTypes))

	for _, st := range ast.StructTypes {
		fields := make([]ir.Param, len(st.Members))
		for i, m := range st.Members {
			fields[i] = paramFromParts(m.GetId(), m.GetTname(), isFileKind(m.IsFile()), m.GetOutName())
		}

		structs[st.Id] = &ir.StructType{Name: st.Id, Fields: fields}
	}

	return structs
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
