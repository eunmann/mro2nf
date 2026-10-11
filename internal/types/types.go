// Package types implements the type-directed file-leaf walk shared by the
// emitter and the runtime shim. Given a parameter's declared type and a decoded
// JSON value, it locates every file-bearing leaf — including files nested inside
// arrays, typed maps, and (arbitrarily deep) structs — and applies a transform
// so the shim can rewrite paths on each task boundary without guessing which
// strings are paths.
package types

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/eunmann/mro2nf/internal/ir"
)

// ErrSkipLeaf is returned by a Transform to resolve that file leaf to null and
// continue the walk instead of aborting it (the leaf analog of
// filepath.SkipDir). It lets publish drop a declared output file the pipeline
// never produced, matching Martian's publish-time "absent file -> null".
var ErrSkipLeaf = errors.New("skip file leaf")

// Martian builtin type names the walks dispatch on.
const (
	typeInt  = "int"
	typeFile = "file"
	typePath = "path"
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

// IsStruct reports whether name is a known struct type. The skeleton-outs
// builder uses it to leave a struct-typed output null (Martian's makeOutArg
// pre-fills only genuine file leaves, nulling complex/struct outputs), since the
// IR's IsFile flag is also set for structs that contain file fields.
func (t *Table) IsStruct(name string) bool {
	_, ok := t.structs[name]

	return ok
}

// OutFilename is the on-disk basename for a declared output, mirroring Martian's
// StructMember.GetOutFilename: an explicit OutName wins; a complex type
// (array/map/struct — per isStruct) or the builtin file/path types use the bare
// name; any other (user) file type appends .<typename>. The pre-populated
// skeleton _outs path a stage writes to and the published path must both use this
// single rule so they never diverge (or an output would vanish at publish time).
func OutFilename(p ir.Param, isStruct func(name string) bool) string {
	switch {
	case p.OutName != "":
		return p.OutName
	case p.ArrayDim > 0 || p.MapDim > 0 || isStruct(p.BaseType):
		return p.Name
	case p.BaseType == typeFile || p.BaseType == typePath:
		return p.Name
	default:
		return p.Name + "." + p.BaseType
	}
}

// Transform maps a file-leaf path to its replacement. Returning an error aborts
// the walk, except ErrSkipLeaf, which resolves that one leaf to null and continues.
type Transform func(path string) (string, error)

// Apply walks each value in vals against the matching param's type, applying fn
// to every file leaf, and returns the rewritten map. Keys without a matching
// param (and values of non-file type) are passed through untouched, so the
// result always preserves the full input, and every Object keeps its key order.
func (t *Table) Apply(params []ir.Param, vals map[string]any, fn Transform) (map[string]any, error) {
	out, err := t.applyObject(params, sortedObject(vals), fn)
	if err != nil {
		return nil, err
	}

	return objectMap(out), nil
}

// CoerceScalars walks vals against params and coerces each int-typed leaf whose
// value is a whole-number float to an integer (e.g. the literal 5.0 bound to an
// int param), matching Martian's int-from-float-literal coercion. This is needed
// because numbers are decoded with json.UseNumber to preserve float fidelity
// (42.0 stays a float), which would otherwise hand a stage 5.0 for an int param.
func (t *Table) CoerceScalars(params []ir.Param, vals map[string]any) map[string]any {
	return t.mapNumbers(params, vals, coerceNumber)
}

// FormatFloats re-encodes each float leaf of vals (a float-typed leaf, or a
// float-literal number inside an untyped map) the way mrp encodes a float
// literal in a stage's _args (Martian's FloatExp uses AppendFloat 'g', -1):
// 2.0 becomes 2 and 1E6 becomes 1e+06. It is for values that stand in for
// .mro literals, such as entry-input overrides; a stage's own outputs keep
// their text, as mrp passes them through unchanged.
func (t *Table) FormatFloats(params []ir.Param, vals map[string]any) map[string]any {
	return t.mapNumbers(params, vals, formatFloat)
}

// NarrowValue returns v with every struct-typed value filtered to exactly its
// declared fields, as mrp's StructType.FilterJson filters a value it resolves
// from a producer's outputs to the receiving type (core/resolve.go Path): extra
// fields are dropped and a missing field is null, recursively through nested
// structs, arrays, and typed maps. A struct Object that needs no filtering
// keeps its key order, and one that does takes its declared field order, as
// FilterJson returns its input unchanged or rewrites it member by member.
// Values of any other shape pass through. v is not mutated.
func (t *Table) NarrowValue(v any, p ir.Param) any {
	nv, _ := t.narrow(v, p.BaseType, p.ArrayDim, p.MapDim)

	return nv
}

// Fields returns the declared fields of struct type name, and whether name is a
// struct type.
func (t *Table) Fields(name string) ([]ir.Param, bool) {
	fields, ok := t.structs[name]

	return fields, ok
}

// applyObject is Apply over an Object, walking its members in params order.
func (t *Table) applyObject(params []ir.Param, o Object, fn Transform) (Object, error) {
	out := slices.Clone(o)

	for _, p := range params {
		i := o.index(p.Name)
		if i < 0 {
			continue
		}

		nv, err := t.walk(o[i].Value, p.BaseType, p.ArrayDim, p.MapDim, p.IsFile, fn)
		if err != nil {
			return nil, fmt.Errorf("param %s: %w", p.Name, err)
		}

		out[i].Value = nv
	}

	return out, nil
}

// mapNumbers returns a copy of vals with every number leaf of a declared param
// replaced by leaf(number, base type).
func (t *Table) mapNumbers(params []ir.Param, vals map[string]any, leaf numberLeaf) map[string]any {
	return objectMap(t.mapObjectNumbers(params, sortedObject(vals), leaf))
}

// mapObjectNumbers is mapNumbers over an Object, keeping its key order.
func (t *Table) mapObjectNumbers(params []ir.Param, o Object, leaf numberLeaf) Object {
	out := slices.Clone(o)

	for _, p := range params {
		if i := o.index(p.Name); i >= 0 {
			out[i].Value = t.mapNumber(o[i].Value, p.BaseType, p.ArrayDim, p.MapDim, leaf)
		}
	}

	return out
}

// numberLeaf maps one number leaf given its declared base type.
type numberLeaf func(n json.Number, base string) any

// mapNumber returns v with its number leaves mapped, never mutating v: nested
// slices and maps are rebuilt copy-on-write so the caller-owned input survives
// intact (mapNumbers' copy-on-return contract).
func (t *Table) mapNumber(v any, base string, arrayDim, mapDim int, leaf numberLeaf) any {
	switch tv := v.(type) {
	case []any:
		if arrayDim <= 0 {
			return v
		}

		out := make([]any, len(tv))
		for i, e := range tv {
			out[i] = t.mapNumber(e, base, arrayDim-1, mapDim, leaf)
		}

		return out
	case map[string]any:
		return asMap(t.mapNumber(sortedObject(tv), base, arrayDim, mapDim, leaf))
	case Object:
		if mapDim > 0 {
			// One typed-map level carrying mapDim-1 inner array dims (see walkMap).
			out := slices.Clone(tv)
			for i, m := range tv {
				out[i].Value = t.mapNumber(m.Value, base, arrayDim+mapDim-1, 0, leaf)
			}

			return out
		}

		if fields, ok := t.structs[base]; ok {
			return t.mapObjectNumbers(fields, tv, leaf)
		}

		if base == untypedMap {
			return mapUntyped(tv, leaf)
		}

		return v
	case json.Number:
		return leaf(tv, base)
	default:
		return v
	}
}

// untypedMap is the base type name of Martian's untyped `map`.
const untypedMap = "map"

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
			return leaf(tv, "float")
		}

		return leaf(tv, untypedMap)
	default:
		return v
	}
}

// formatFloat re-encodes a float param's number in Martian's 'g', -1 form. A
// number outside float64 range is left as written.
func formatFloat(n json.Number, base string) any {
	if base != "float" {
		return n
	}

	f, err := strconv.ParseFloat(string(n), 64)
	if err != nil {
		return n
	}

	return json.Number(strconv.FormatFloat(f, 'g', -1, 64))
}

// coerceNumber converts a whole-number JSON value to an integer for an int param.
func coerceNumber(n json.Number, base string) any {
	if base != typeInt {
		return n
	}

	if i, err := n.Int64(); err == nil {
		return i
	}

	// A whole-number float in int64 range coerces to an integer; out-of-range
	// values (e.g. 1e20) are left as the original number rather than overflowing
	// into a garbage int. The upper bound is strict: float64(math.MaxInt64)
	// rounds up to 2^63, so `<= MaxInt64` would admit exactly 2^63, which
	// int64() then wraps to MinInt64; `< MaxInt64` rejects it. MinInt64 (-2^63)
	// is exactly representable, so the lower bound stays inclusive.
	if f, err := n.Float64(); err == nil && f == math.Trunc(f) &&
		f >= math.MinInt64 && f < math.MaxInt64 {
		return int64(f)
	}

	return n
}

// narrow filters one value against its type and reports whether filtering
// changed it; it descends shapes the way coerce does (a typed map is one level
// carrying mapDim-1 inner array dims).
func (t *Table) narrow(v any, base string, arrayDim, mapDim int) (any, bool) {
	switch tv := v.(type) {
	case []any:
		if arrayDim <= 0 {
			return v, false
		}

		out := make([]any, len(tv))
		changed := false

		for i, e := range tv {
			var c bool

			out[i], c = t.narrow(e, base, arrayDim-1, mapDim)
			changed = changed || c
		}

		return out, changed
	case map[string]any:
		nv, changed := t.narrowObject(sortedObject(tv), base, arrayDim, mapDim)

		return asMap(nv), changed
	case Object:
		return t.narrowObject(tv, base, arrayDim, mapDim)
	default:
		return v, false
	}
}

// narrowObject narrows a typed map's values in place, or filters a struct as
// StructType.FilterJson does: unchanged when it has exactly its declared
// fields and none of them changes, otherwise rebuilt in declared order.
func (t *Table) narrowObject(o Object, base string, arrayDim, mapDim int) (any, bool) {
	if mapDim > 0 {
		out := slices.Clone(o)
		changed := false

		for i, m := range o {
			var c bool

			out[i].Value, c = t.narrow(m.Value, base, arrayDim+mapDim-1, 0)
			changed = changed || c
		}

		return out, changed
	}

	fields, ok := t.structs[base]
	if !ok || arrayDim > 0 {
		return o, false
	}

	out := make(Object, len(fields))
	changed := len(o) != len(fields)

	for i, f := range fields {
		v, ok := o.Get(f.Name)
		nv, c := t.narrow(v, f.BaseType, f.ArrayDim, f.MapDim)
		changed = changed || !ok || c
		out[i] = Member{Key: f.Name, Value: nv}
	}

	if !changed {
		return o, false
	}

	return out, true
}

// walk descends one value. It dispatches on the value's actual JSON shape
// rather than on a fixed dimension order, so both map<T[]> (a map of arrays) and
// the array-of-typed-map shape resolve correctly even though the IR flattens
// ArrayDim/MapDim and loses their nesting order. Typed maps are walked in
// sorted key order so the resulting file-leaf layout (and the markers written
// into the bundle) do not depend on how a producer ordered its keys, keeping
// -resume caching stable; every object keeps its own key order. Shape
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
		nv, err := t.walk(sortedObject(tv), base, arrayDim, mapDim, isFile, fn)

		return asMap(nv), err
	case Object:
		if mapDim > 0 {
			return t.walkMap(tv, base, arrayDim, mapDim, isFile, fn)
		}

		// A struct value descends regardless of isFile: Martian marks a struct
		// that contains file fields as a directory kind (isFile true), but the
		// struct itself is an object to recurse into, not a file leaf. (A real
		// file leaf is a string, handled below, never an object.)
		if fields, ok := t.structs[base]; ok {
			return t.applyObject(fields, tv, fn)
		}

		return v, nil
	case string:
		if isFile && arrayDim == 0 && mapDim == 0 {
			nv, err := fn(tv)
			if errors.Is(err, ErrSkipLeaf) {
				return nil, nil
			}

			return nv, err
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

func (t *Table) walkMap(m Object, base string, arrayDim, mapDim int, isFile bool, fn Transform) (any, error) {
	out := slices.Clone(m)

	// A typed map is exactly one map level; Martian folds any inner array
	// dimensions into MapDim (MapDim = 1 + innerArrayDim), so after descending
	// this level the element carries mapDim-1 array dims (and never another typed
	// map — nested typed maps are not representable). Treating mapDim as a count
	// of nested map levels would leave map<T[]> inner arrays unwalked.
	for _, i := range sortedIndices(m) {
		nv, err := t.walk(m[i].Value, base, arrayDim+mapDim-1, 0, isFile, fn)
		if err != nil {
			return nil, err
		}

		out[i].Value = nv
	}

	return out, nil
}
