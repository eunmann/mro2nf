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
	"sort"

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

// walk descends one value. It dispatches on the value's actual JSON shape
// rather than on a fixed dimension order, so both map<T[]> (a map of arrays) and
// the array-of-typed-map shape resolve correctly even though the IR flattens
// ArrayDim/MapDim and loses their nesting order. Maps are walked in sorted key
// order so the resulting file-leaf layout (and the markers written into the
// bundle) are deterministic across runs, keeping -resume caching stable. Shape
// mismatches (e.g. a null, or a json.Number where a file string is expected)
// pass through untouched, since runtime values may legitimately be null.
func (t *Table) walk(v any, base string, arrayDim, mapDim int, isFile bool, fn Transform) (any, error) {
	switch tv := v.(type) {
	case nil:
		return nil, nil
	case []any:
		if arrayDim <= 0 {
			return v, nil
		}

		return t.walkSlice(tv, base, arrayDim, mapDim, isFile, fn)
	case map[string]any:
		if mapDim > 0 {
			return t.walkMap(tv, base, arrayDim, mapDim, isFile, fn)
		}

		if fields, ok := t.structs[base]; ok && !isFile {
			return t.Apply(fields, tv, fn)
		}

		return v, nil
	case string:
		if isFile && arrayDim == 0 && mapDim == 0 {
			return fn(tv)
		}

		return v, nil
	default:
		return v, nil
	}
}

func (t *Table) walkSlice(arr []any, base string, arrayDim, mapDim int, isFile bool, fn Transform) (any, error) {
	out := make([]any, len(arr))

	for i, e := range arr {
		nv, err := t.walk(e, base, arrayDim-1, mapDim, isFile, fn)
		if err != nil {
			return nil, err
		}

		out[i] = nv
	}

	return out, nil
}

func (t *Table) walkMap(m map[string]any, base string, arrayDim, mapDim int, isFile bool, fn Transform) (any, error) {
	out := make(map[string]any, len(m))

	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	for _, k := range keys {
		nv, err := t.walk(m[k], base, arrayDim, mapDim-1, isFile, fn)
		if err != nil {
			return nil, err
		}

		out[k] = nv
	}

	return out, nil
}
