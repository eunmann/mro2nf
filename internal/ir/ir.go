// Package ir defines the transpiler's intermediate representation: a
// normalized, martian/syntax-independent model of a Martian program that the
// emitter and shim consume.
package ir

import "encoding/json"

// Lang identifies a stage adapter language.
type Lang string

// The three Martian stage adapter languages.
const (
	LangPy   Lang = "py"
	LangExec Lang = "exec"
	LangComp Lang = "comp"
)

// Param is a single stage or pipeline input/output parameter. The json tags let
// it round-trip through the runtime type manifest.
type Param struct {
	// Name is the parameter identifier.
	Name string `json:"name"`
	// Type is the rendered MRO type, e.g. "float", "float[]", "map<int>".
	Type string `json:"type"`
	// BaseType is the bare element type name with array/map wrappers stripped,
	// e.g. "float", "file", or a struct name like "Cfg". Used to resolve struct
	// fields during the type-directed file-leaf walk.
	BaseType string `json:"baseType"`
	// ArrayDim is the number of array dimensions (0 for a scalar).
	ArrayDim int `json:"arrayDim"`
	// MapDim is the number of typed-map dimensions (0 for a non-map).
	MapDim int `json:"mapDim"`
	// IsFile reports whether the type refers to a file, directory, or path.
	IsFile bool `json:"isFile"`
	// OutName is the optional on-disk output filename (output params only).
	OutName string `json:"outName,omitempty"`
}

// StructType is a Martian struct: an ordered, named set of typed fields. Stage
// and pipeline output params whose BaseType names a struct expand into these
// fields during the file-leaf walk.
type StructType struct {
	// Name is the struct type identifier.
	Name string `json:"name"`
	// Fields are the struct's members, each a typed Param.
	Fields []Param `json:"fields"`
}

// Resources is a stage's static resource request from `using(...)`.
type Resources struct {
	MemGB   float64
	VMemGB  float64
	Threads float64
	Special string
}

// Stage is a normalized Martian stage declaration.
type Stage struct {
	// Name is the stage identifier.
	Name string
	// In and Out are the top-level input and output parameters.
	In  []Param
	Out []Param
	// Split reports whether the stage chunks (has split/main/join phases).
	Split bool
	// ChunkIn and ChunkOut are the per-chunk parameters declared in `split`.
	ChunkIn  []Param
	ChunkOut []Param
	// Lang, SrcPath, and SrcArgs locate and identify the stage adapter code.
	Lang    Lang
	SrcPath string
	SrcArgs []string
	// Resources is the static resource request.
	Resources Resources
}

// Ref is a reference expression: a pipeline input (self) or a call output.
type Ref struct {
	// Kind is "self" (pipeline input) or "call" (another call's output).
	Kind string
	// ID is the pipeline input name (self) or call instance id (call).
	ID string
	// Output is the binding path within the referent, e.g. "sum" or "a.b".
	// Empty means the whole output struct.
	Output string
}

// Value is a binding's value expression: a leaf literal or ref, or a composite
// array/object whose elements may themselves contain refs (e.g. a fan-in
// `[A.out, B.out]` or `{"k": UP.out}`). Exactly one field is set.
type Value struct {
	// Literal is a JSON-encoded constant leaf.
	Literal json.RawMessage
	// Ref is a reference leaf (pipeline input or upstream output).
	Ref *Ref
	// Array is an array literal whose elements are themselves values.
	Array []Value
	// Object is a map/struct literal whose values are themselves values.
	Object map[string]Value
}

// Binding assigns a value expression to a callee input or pipeline output.
type Binding struct {
	// Param is the bound parameter name ("*" for a wildcard binding).
	Param string
	// Value is the bound expression.
	Value Value
	// Split marks a `split` binding in a map call: the value is a collection
	// to fork over, one element per fork.
	Split bool
}

// Call is an invocation of a stage or pipeline within a pipeline.
type Call struct {
	// Name is the call instance id; Callable is the declared callable name.
	Name     string
	Callable string
	// Bindings wire the callee's inputs.
	Bindings []Binding
	// Disabled, when set, conditionally skips the call at runtime.
	Disabled *Ref
	// Local, Preflight, and Volatile are compile-time call modifiers.
	Local     bool
	Preflight bool
	Volatile  bool
	// Mapped reports a `map call ... split` fork over a collection.
	Mapped bool
}

// Pipeline is a normalized Martian pipeline declaration.
type Pipeline struct {
	// Name is the pipeline identifier.
	Name string
	// In and Out are the pipeline's input and output parameters.
	In  []Param
	Out []Param
	// Calls are the pipeline's call statements in source order.
	Calls []Call
	// Returns binds the pipeline's outputs.
	Returns []Binding
}

// EntryCall is the top-level call that invokes the entry pipeline or stage.
type EntryCall struct {
	// Callable is the entry pipeline/stage name.
	Callable string
	// Bindings are the entry inputs (literals or refs to nothing).
	Bindings []Binding
}

// Program is the whole transpiler input: all stages, pipelines, and the entry.
type Program struct {
	// Stages and Pipelines are keyed by name.
	Stages    map[string]*Stage
	Pipelines map[string]*Pipeline
	// Structs holds explicit `struct` type declarations, keyed by name, so the
	// file-leaf walk can expand nested struct-typed values.
	Structs map[string]*StructType
	// Entry is the top-level call, or nil if the source declares none.
	Entry *EntryCall
}
