package gen

import (
	"encoding/json"
	"fmt"
	"slices"
)

// Program is one generated Martian program: its declarations, its pipelines
// (each after every pipeline it calls; the last is the entry pipeline) and the
// literal bindings of the entry call. References are by name, so a Program
// round-trips through JSON, which Clone relies on.
type Program struct {
	Seed      uint64      `json:"seed"`
	Attempt   uint64      `json:"attempt"`
	FileTypes []string    `json:"filetypes,omitempty"`
	Structs   []*Struct   `json:"structs,omitempty"`
	Stages    []*Stage    `json:"stages,omitempty"`
	Pipelines []*Pipeline `json:"pipelines"`
	Entry     []*Binding  `json:"entry,omitempty"`
}

// Param is a named, typed parameter, struct field or output.
type Param struct {
	Name string `json:"name"`
	Type Type   `json:"type"`
}

// Struct is a user struct declaration.
type Struct struct {
	Name   string  `json:"name"`
	Fields []Param `json:"fields"`
}

// Stage is a py stage declaration. ChunkIn and ChunkOut are its split
// parameters when Split is set.
type Stage struct {
	Name     string  `json:"name"`
	In       []Param `json:"in,omitempty"`
	Out      []Param `json:"out,omitempty"`
	Split    bool    `json:"split,omitempty"`
	ChunkIn  []Param `json:"chunk_in,omitempty"`
	ChunkOut []Param `json:"chunk_out,omitempty"`
	Fail     *Fail   `json:"fail,omitempty"`
}

// Fail makes a stage fail in Phase whenever the hash of its inputs is a
// multiple of Mod, so whether it fails depends on the data it received.
type Fail struct {
	Kind  string `json:"kind"`
	Phase string `json:"phase"`
	Mod   int    `json:"mod"`
}

// Fail kinds: an ASSERT (martian.exit) or an ordinary exception.
const (
	FailAssert = "assert"
	FailError  = "error"
)

// Pipeline is a pipeline declaration.
type Pipeline struct {
	Name   string     `json:"name"`
	In     []Param    `json:"in,omitempty"`
	Out    []Param    `json:"out,omitempty"`
	Calls  []*Call    `json:"calls,omitempty"`
	Return []*Binding `json:"return,omitempty"`
}

// Call is a call statement. It is a map call when any binding splits.
// Wildcard, when set, is the `* = <ref>` binding; Disabled the disable ref.
type Call struct {
	Callee   string     `json:"callee"`
	ID       string     `json:"id"`
	Bindings []*Binding `json:"bindings,omitempty"`
	Wildcard *Expr      `json:"wildcard,omitempty"`
	Disabled *Expr      `json:"disabled,omitempty"`
}

// IsMap reports whether c is a map call.
func (c *Call) IsMap() bool {
	return slices.ContainsFunc(c.Bindings, func(b *Binding) bool { return b.Split })
}

// Binding binds Name to Value, splitting it when Split is set.
type Binding struct {
	Name  string `json:"name"`
	Split bool   `json:"split,omitempty"`
	Value *Expr  `json:"value"`
}

// ExprKind is the form of an expression.
type ExprKind string

// Expression kinds. ExprStruct is a struct literal, whose Keys are field names.
const (
	ExprNull   ExprKind = "null"
	ExprInt    ExprKind = "int"
	ExprFloat  ExprKind = "float"
	ExprString ExprKind = "string"
	ExprBool   ExprKind = "bool"
	ExprArray  ExprKind = "array"
	ExprMap    ExprKind = "map"
	ExprStruct ExprKind = "struct"
	ExprRef    ExprKind = "ref"
)

// Expr is a binding expression: a literal, a collection literal whose
// elements may be refs (a fan-in), or a ref. A scalar literal's Text is its
// MRO token (a string's unquoted value). A ref names "self" or a call ID, then
// the output (or input, for self) and the fields it projects.
type Expr struct {
	Kind  ExprKind `json:"kind"`
	Text  string   `json:"text,omitempty"`
	Elems []*Expr  `json:"elems,omitempty"`
	Keys  []string `json:"keys,omitempty"`
	Ref   string   `json:"ref,omitempty"`
	Path  []string `json:"path,omitempty"`
}

const selfRef = "self"

func null() *Expr { return &Expr{Kind: ExprNull} }

func ref(id string, path ...string) *Expr {
	return &Expr{Kind: ExprRef, Ref: id, Path: path}
}

// walk calls visit for e and every expression nested in it, parents first.
func (e *Expr) walk(visit func(*Expr)) {
	if e == nil {
		return
	}

	visit(e)

	for _, c := range e.Elems {
		c.walk(visit)
	}
}

// Clone deep-copies p.
func (p *Program) Clone() *Program {
	data, err := json.Marshal(p)
	if err != nil {
		panic(fmt.Sprintf("marshal program: %v", err))
	}

	var c Program
	if err := json.Unmarshal(data, &c); err != nil {
		panic(fmt.Sprintf("unmarshal program: %v", err))
	}

	return &c
}

// Size summarizes how big p is, for reports.
func Size(p *Program) string {
	calls := 0
	for _, pl := range p.Pipelines {
		calls += len(pl.Calls)
	}

	return fmt.Sprintf("%d stages, %d pipelines, %d calls", len(p.Stages), len(p.Pipelines), calls)
}

// EntryPipeline is the pipeline the entry call invokes.
func (p *Program) EntryPipeline() *Pipeline { return p.Pipelines[len(p.Pipelines)-1] }

func (p *Program) stage(name string) *Stage {
	for _, s := range p.Stages {
		if s.Name == name {
			return s
		}
	}

	return nil
}

func (p *Program) pipeline(name string) *Pipeline {
	for _, pl := range p.Pipelines {
		if pl.Name == name {
			return pl
		}
	}

	return nil
}

// calleeIO is the in and out params of the stage or pipeline called name.
func (p *Program) calleeIO(name string) ([]Param, []Param) {
	if s := p.stage(name); s != nil {
		return s.In, s.Out
	}

	if pl := p.pipeline(name); pl != nil {
		return pl.In, pl.Out
	}

	return nil, nil
}

// fields is the members of the struct type called name: a user struct's
// fields or, for a callable used as a type, its outputs.
func (p *Program) fields(name string) ([]Param, bool) {
	for _, s := range p.Structs {
		if s.Name == name {
			return s.Fields, true
		}
	}

	if s := p.stage(name); s != nil {
		return s.Out, true
	}

	if pl := p.pipeline(name); pl != nil {
		return pl.Out, true
	}

	return nil, false
}

// reads reports whether e reads a ref match accepts.
func reads(e *Expr, match func(*Expr) bool) bool {
	found := false

	e.walk(func(x *Expr) { found = found || (x.Kind == ExprRef && match(x)) })

	return found
}

// callRead reports whether a binding, gate, wildcard or return of pl reads
// the call id.
func callRead(pl *Pipeline, id string) bool {
	byID := func(e *Expr) bool { return e.Ref == id }

	for _, c := range pl.Calls {
		for _, b := range c.Bindings {
			if reads(b.Value, byID) {
				return true
			}
		}

		if (c.Disabled != nil && byID(c.Disabled)) || (c.Wildcard != nil && byID(c.Wildcard)) {
			return true
		}
	}

	return slices.ContainsFunc(pl.Return, func(b *Binding) bool { return reads(b.Value, byID) })
}

// inputUsed reports whether a binding, gate or return of pl reads its input
// name, as Martian requires of every pipeline input.
func (p *Program) inputUsed(pl *Pipeline, name string) bool {
	used := false
	visit := func(e *Expr) {
		e.walk(func(x *Expr) {
			if x.Kind == ExprRef && x.Ref == selfRef && len(x.Path) > 0 && x.Path[0] == name {
				used = true
			}
		})
	}

	for _, c := range pl.Calls {
		for _, b := range c.Bindings {
			visit(b.Value)
		}

		visit(c.Disabled)

		if c.Wildcard != nil && c.Wildcard.Ref == selfRef {
			in, _ := p.calleeIO(c.Callee)
			used = used || paramIndex(in, name) >= 0
		}
	}

	for _, b := range pl.Return {
		visit(b.Value)
	}

	return used
}

func paramIndex(ps []Param, name string) int {
	return slices.IndexFunc(ps, func(q Param) bool { return q.Name == name })
}
