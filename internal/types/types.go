// Package types implements the type-directed file-leaf walk mre runs at every
// task boundary. Given a parameter's type in Martian's compiled type table and
// a decoded JSON value, it locates every file-bearing leaf — including files
// nested inside arrays, typed maps, and (arbitrarily deep) structs — and
// applies a transform so the shim can rewrite paths on each task boundary
// without guessing which strings are paths.
package types

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

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

// FormatFloats re-encodes each float leaf of vals (a float-typed leaf, or a
// float-literal number inside an untyped map) the way mrp encodes a float
// literal in a stage's _args (Martian's FloatExp uses AppendFloat 'g', -1):
// 2.0 becomes 2 and 1E6 becomes 1e+06. It is for values that stand in for
// .mro literals, such as entry-input overrides; a stage's own outputs keep
// their text, as mrp passes them through unchanged.
func (t *Table) FormatFloats(params []*syntax.StructMember, vals map[string]any) (map[string]any, error) {
	out, err := t.mapObjectNumbers(params, sortedObject(vals), formatFloat)
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

// numberLeaf maps one number leaf given the name of its declared base type.
type numberLeaf func(n json.Number, base string) any

// mapObjectNumbers returns o with the number leaves of each declared param
// mapped by leaf, keeping o's key order.
func (t *Table) mapObjectNumbers(params []*syntax.StructMember, o Object, leaf numberLeaf) (Object, error) {
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

		if out[i].Value, err = t.mapNumber(o[i].Value, typ, leaf); err != nil {
			return nil, fmt.Errorf("param %s: %w", p.Id, err)
		}
	}

	return out, nil
}

// mapNumber returns v with its number leaves mapped, never mutating v: nested
// slices and maps are rebuilt copy-on-write so the caller-owned input survives
// intact. A number is mapped by the base type name of the type it is found at.
func (t *Table) mapNumber(v any, typ syntax.Type, leaf numberLeaf) (any, error) {
	switch tv := v.(type) {
	case []any:
		at, ok := typ.(*syntax.ArrayType)
		if !ok {
			return v, nil
		}

		elem := t.elemType(at)
		out := make([]any, len(tv))

		for i, e := range tv {
			var err error
			if out[i], err = t.mapNumber(e, elem, leaf); err != nil {
				return nil, err
			}
		}

		return out, nil
	case map[string]any:
		nv, err := t.mapNumber(sortedObject(tv), typ, leaf)

		return asMap(nv), err
	case Object:
		return t.mapObjectNumber(tv, typ, leaf)
	case json.Number:
		return leaf(tv, typ.TypeId().Tname), nil
	default:
		return v, nil
	}
}

// mapObjectNumber is mapNumber over an object value.
func (t *Table) mapObjectNumber(o Object, typ syntax.Type, leaf numberLeaf) (any, error) {
	switch ot := typ.(type) {
	case *syntax.TypedMapType:
		out := slices.Clone(o)

		for i, m := range o {
			var err error
			if out[i].Value, err = t.mapNumber(m.Value, ot.Elem, leaf); err != nil {
				return nil, err
			}
		}

		return out, nil
	case *syntax.StructType:
		return t.mapObjectNumbers(ot.Members, o, leaf)
	case *syntax.BuiltinType:
		if ot.Id == syntax.KindMap {
			return mapUntyped(o, leaf), nil
		}
	}

	return o, nil
}

// mapUntyped returns v, a value inside an untyped map, with its number leaves
// mapped at any depth. Martian types such a number by its literal: one written
// with a '.' or an exponent is a float, any other keeps the untyped base.
func mapUntyped(v any, leaf numberLeaf) any {
	switch tv := v.(type) {
	case []any:
		out := make([]any, len(tv))
		for i, e := range tv {
			out[i] = mapUntyped(e, leaf)
		}

		return out
	case map[string]any:
		return asMap(mapUntyped(sortedObject(tv), leaf))
	case Object:
		out := slices.Clone(tv)
		for i, m := range tv {
			out[i].Value = mapUntyped(m.Value, leaf)
		}

		return out
	case json.Number:
		if strings.ContainsAny(string(tv), ".eE") {
			return leaf(tv, syntax.KindFloat)
		}

		return leaf(tv, syntax.KindMap)
	default:
		return v
	}
}

// formatFloat re-encodes a float param's number in Martian's 'g', -1 form. A
// number outside float64 range is left as written.
func formatFloat(n json.Number, base string) any {
	if base != syntax.KindFloat {
		return n
	}

	f, err := strconv.ParseFloat(string(n), 64)
	if err != nil {
		return n
	}

	return json.Number(strconv.FormatFloat(f, 'g', -1, 64))
}
