// Package frontend lowers a Martian AST, compiled by Martian's own syntax
// package, into the typed parameters the type manifest is built from.
package frontend

import (
	"strings"

	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/martian-lang/martian/martian/syntax"
)

// Lower returns the parameters of every stage, pipeline and struct a
// compiled Martian AST declares.
func Lower(ast *syntax.Ast) *ir.Program {
	prog := &ir.Program{
		Stages:    make(map[string]*ir.Stage, len(ast.Stages)),
		Pipelines: make(map[string]*ir.Pipeline, len(ast.Pipelines)),
		Structs:   lowerStructs(ast),
	}

	for _, s := range ast.Stages {
		prog.Stages[s.Id] = &ir.Stage{
			Name:     s.Id,
			In:       lowerInParams(s.InParams),
			Out:      lowerOutParams(s.OutParams),
			ChunkIn:  lowerInParams(s.ChunkIns),
			ChunkOut: lowerOutParams(s.ChunkOuts),
		}
	}

	for _, p := range ast.Pipelines {
		prog.Pipelines[p.Id] = &ir.Pipeline{
			Name: p.Id,
			In:   lowerInParams(p.InParams),
			Out:  lowerOutParams(p.OutParams),
		}
	}

	addCallableStructs(prog)

	return prog
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

// addCallableStructs registers a struct type for every stage and pipeline with
// outputs, keyed by the callable name — mirroring Martian's structFromCallable.
// Martian makes a param typed by a callable name (`out MYSTAGE made`) a legal
// struct whose fields are that callable's outputs; without registering it here
// the type-walk would treat the param as an opaque leaf and never rewrite,
// stage, or publish the file paths inside it. An explicit `struct` decl of the
// same name (which Martian's shared type namespace would already have rejected)
// keeps precedence.
func addCallableStructs(prog *ir.Program) {
	add := func(name string, out []ir.Param) {
		if len(out) == 0 {
			return
		}

		if _, exists := prog.Structs[name]; exists {
			return
		}

		fields := make([]ir.Param, len(out))
		copy(fields, out)
		prog.Structs[name] = &ir.StructType{Name: name, Fields: fields}
	}

	for _, s := range prog.Stages {
		add(s.Name, s.Out)
	}

	for _, p := range prog.Pipelines {
		add(p.Name, p.Out)
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
