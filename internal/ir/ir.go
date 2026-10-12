// Package ir defines the typed parameters of a Martian program's callables and
// structs, a martian/syntax-independent model the type manifest is built from.
package ir

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

// Stage is a Martian stage's parameters.
type Stage struct {
	// Name is the stage identifier.
	Name string
	// In and Out are the top-level input and output parameters.
	In  []Param
	Out []Param
	// ChunkIn and ChunkOut are the per-chunk parameters declared in `split`.
	ChunkIn  []Param
	ChunkOut []Param
}

// Pipeline is a Martian pipeline's parameters.
type Pipeline struct {
	// Name is the pipeline identifier.
	Name string
	// In and Out are the pipeline's input and output parameters.
	In  []Param
	Out []Param
}

// Program is every callable and struct of a Martian program.
type Program struct {
	// Stages and Pipelines are keyed by name.
	Stages    map[string]*Stage
	Pipelines map[string]*Pipeline
	// Structs holds explicit `struct` type declarations, keyed by name, so the
	// file-leaf walk can expand nested struct-typed values.
	Structs map[string]*StructType
}
