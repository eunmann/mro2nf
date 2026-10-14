// Package types implements the type-directed file-leaf walk mre runs at every
// task boundary. Given a parameter's type in Martian's compiled type table and
// a decoded JSON value, it locates every file-bearing leaf — including files
// nested inside arrays, typed maps, and (arbitrarily deep) structs — and
// applies a transform so the shim can rewrite paths on each task boundary
// without guessing which strings are paths.
package types

import (
	"errors"
	"fmt"
	"slices"

	"github.com/martian-lang/martian/martian/syntax"
)

var (
	// ErrSkipLeaf is returned by a Transform to resolve that file leaf to null
	// and continue the walk instead of aborting it (the leaf analog of
	// filepath.SkipDir). It lets publish drop a declared output file the
	// pipeline never produced, matching Martian's publish-time "absent file ->
	// null".
	ErrSkipLeaf = errors.New("skip file leaf")
	// ErrUnknownType reports a parameter whose type the type table lacks.
	ErrUnknownType = errors.New("type not in the type table")
)

// Table is the walk over the types of a compiled Martian program: its
// filetypes, its structs, and the struct Martian registers for each callable's
// outputs (syntax/compile_types.go CompileTypes).
type Table struct {
	lookup *syntax.TypeLookup
}

// NewTable returns the walk over the types lookup holds.
func NewTable(lookup *syntax.TypeLookup) *Table {
	return &Table{lookup: lookup}
}

// Type returns the type id names.
func (t *Table) Type(id syntax.TypeId) (syntax.Type, error) {
	if typ := t.lookup.Get(id); typ != nil {
		return typ, nil
	}

	return nil, fmt.Errorf("%w: %s", ErrUnknownType, id.String())
}

// HasFiles reports whether a value of the type id names can hold a file
// leaf, the leaves Apply walks: Martian marks a file, a directory of files,
// and an array, a typed map or a struct that holds one.
func (t *Table) HasFiles(id syntax.TypeId) (bool, error) {
	typ, err := t.Type(id)
	if err != nil {
		return false, err
	}

	return isFileKind(typ.IsFile()), nil
}

// Transform maps a file-leaf path to its replacement. Returning an error aborts
// the walk, except ErrSkipLeaf, which resolves that one leaf to null and continues.
type Transform func(path string) (string, error)

// Apply walks each value in vals against the matching param's type, applying fn
// to every file leaf, and returns the rewritten map. Keys without a matching
// param (and values of non-file type) are passed through untouched, so the
// result always preserves the full input, and every Object keeps its key order.
func (t *Table) Apply(params []*syntax.StructMember, vals map[string]any, fn Transform) (map[string]any, error) {
	out, err := t.applyObject(params, sortedObject(vals), fn)
	if err != nil {
		return nil, err
	}

	return objectMap(out), nil
}

// applyObject is Apply over an Object, walking its members in params order.
func (t *Table) applyObject(params []*syntax.StructMember, o Object, fn Transform) (Object, error) {
	out := slices.Clone(o)

	for _, p := range params {
		i := o.index(p.Id)
		if i < 0 {
			continue
		}

		typ, err := t.Type(p.Tname)
		if err != nil {
			return nil, fmt.Errorf("param %s: %w", p.Id, err)
		}

		nv, err := t.walk(o[i].Value, typ, fn)
		if err != nil {
			return nil, fmt.Errorf("param %s: %w", p.Id, err)
		}

		out[i].Value = nv
	}

	return out, nil
}

// elemType is the type of an element of the array type at: Martian's
// ArrayType holds its innermost element type and a dimension count.
func (t *Table) elemType(at *syntax.ArrayType) syntax.Type {
	return t.lookup.GetArray(at.Elem, at.Dim-1)
}

// isFileKind reports whether a file kind denotes a file or a directory of
// files. KindMayContainPaths (a string, an untyped map) is a VDR heuristic,
// not a file.
func isFileKind(fk syntax.FileKind) bool {
	return fk == syntax.KindIsFile || fk == syntax.KindIsDirectory
}

// walk descends one value along its type. Typed maps are walked in sorted key
// order so the resulting file-leaf layout (and the markers written into the
// bundle) do not depend on how a producer ordered its keys, keeping -resume
// caching stable; every object keeps its own key order. A value whose shape
// disagrees with its type (e.g. a null, or a json.Number where a file string
// is declared) passes through untouched, since runtime values may
// legitimately be null.
func (t *Table) walk(v any, typ syntax.Type, fn Transform) (any, error) {
	switch tv := v.(type) {
	case []any:
		at, ok := typ.(*syntax.ArrayType)
		if !ok {
			return v, nil
		}

		return t.walkSlice(tv, t.elemType(at), fn)
	case map[string]any:
		nv, err := t.walk(sortedObject(tv), typ, fn)

		return asMap(nv), err
	case Object:
		switch ot := typ.(type) {
		case *syntax.TypedMapType:
			return t.walkMap(tv, ot.Elem, fn)
		case *syntax.StructType:
			return t.applyObject(ot.Members, tv, fn)
		default:
			return v, nil
		}
	case string:
		return t.walkString(tv, typ, fn)
	default:
		return v, nil
	}
}

// walkString applies fn to s when typ is a file leaf type. Martian marks a
// struct that contains file fields as a directory kind, so a string where
// such a struct is declared is a leaf too.
func (t *Table) walkString(s string, typ syntax.Type, fn Transform) (any, error) {
	switch typ.(type) {
	case *syntax.ArrayType, *syntax.TypedMapType:
		return s, nil
	}

	if !isFileKind(typ.IsFile()) {
		return s, nil
	}

	nv, err := fn(s)
	if errors.Is(err, ErrSkipLeaf) {
		return nil, nil
	}

	return nv, err
}

func (t *Table) walkSlice(arr []any, elem syntax.Type, fn Transform) (any, error) {
	out := make([]any, len(arr))

	for i, e := range arr {
		nv, err := t.walk(e, elem, fn)
		if err != nil {
			return nil, err
		}

		out[i] = nv
	}

	return out, nil
}

func (t *Table) walkMap(m Object, elem syntax.Type, fn Transform) (any, error) {
	out := slices.Clone(m)

	for _, i := range sortedIndices(m) {
		nv, err := t.walk(m[i].Value, elem, fn)
		if err != nil {
			return nil, err
		}

		out[i].Value = nv
	}

	return out, nil
}
