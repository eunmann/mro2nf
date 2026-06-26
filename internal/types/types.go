// Package types implements the type-directed file-leaf walk shared by the
// emitter and the runtime shim. Given a parameter's declared type and a decoded
// JSON value, it locates every file-bearing leaf — including files nested inside
// arrays, typed maps, and (arbitrarily deep) structs — and applies a transform
// so the shim can rewrite paths on each task boundary without guessing which
// strings are paths.
package types

import (
	"fmt"
	"maps"

	"github.com/eunmann/martian-nextflow/internal/ir"
)

// Table resolves struct type names to their fields so the walk can descend into
// struct-typed values.
type Table struct {
	structs map[string][]ir.Param
}

// NewTable builds a Table from a program's struct definitions.
func NewTable(structs map[string]*ir.StructType) *Table {
	m := make(map[string][]ir.Param, len(structs))
	for name, st := range structs {
		m[name] = st.Fields
	}

	return &Table{structs: m}
}

// Transform maps a file-leaf path to its replacement. Returning an error aborts
// the walk.
type Transform func(path string) (string, error)

// Apply walks each value in vals against the matching param's type, applying fn
// to every file leaf, and returns the rewritten map. Keys without a matching
// param (and values of non-file type) are passed through untouched, so the
// result always preserves the full input.
func (t *Table) Apply(params []ir.Param, vals map[string]any, fn Transform) (map[string]any, error) {
	out := make(map[string]any, len(vals))
	maps.Copy(out, vals)

	for _, p := range params {
		v, ok := vals[p.Name]
		if !ok {
			continue
		}

		nv, err := t.walk(v, p.BaseType, p.ArrayDim, p.MapDim, p.IsFile, fn)
		if err != nil {
			return nil, fmt.Errorf("param %s: %w", p.Name, err)
		}

		out[p.Name] = nv
	}

	return out, nil
}

// walk descends one value. Typed-map dimensions are peeled before array
// dimensions (so map<T[]> — a map of arrays — resolves correctly); the rare
// array-of-typed-map shape is not produced by the Martian grammar in practice.
// Shape mismatches (e.g. a null where a map is expected) pass through untouched
// rather than erroring, since runtime values may legitimately be null.
func (t *Table) walk(v any, base string, arrayDim, mapDim int, isFile bool, fn Transform) (any, error) {
	if v == nil {
		return nil, nil
	}

	if mapDim > 0 {
		return t.walkMap(v, base, arrayDim, mapDim, isFile, fn)
	}

	if arrayDim > 0 {
		return t.walkArray(v, base, arrayDim, isFile, fn)
	}

	if isFile {
		s, ok := v.(string)
		if !ok {
			return v, nil
		}

		return fn(s)
	}

	if fields, ok := t.structs[base]; ok {
		m, ok := v.(map[string]any)
		if !ok {
			return v, nil
		}

		return t.Apply(fields, m, fn)
	}

	return v, nil
}

func (t *Table) walkMap(v any, base string, arrayDim, mapDim int, isFile bool, fn Transform) (any, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return v, nil
	}

	out := make(map[string]any, len(m))
	for k, e := range m {
		nv, err := t.walk(e, base, arrayDim, mapDim-1, isFile, fn)
		if err != nil {
			return nil, err
		}

		out[k] = nv
	}

	return out, nil
}

func (t *Table) walkArray(v any, base string, arrayDim int, isFile bool, fn Transform) (any, error) {
	arr, ok := v.([]any)
	if !ok {
		return v, nil
	}

	out := make([]any, len(arr))
	for i, e := range arr {
		nv, err := t.walk(e, base, arrayDim-1, 0, isFile, fn)
		if err != nil {
			return nil, err
		}

		out[i] = nv
	}

	return out, nil
}
