package bind

import (
	"maps"
	"slices"

	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/types"
)

// Narrow filters each resolved arg to its receiving param's type where mrp
// filters it, so the arg also has mrp's key order (core/resolve.go):
//   - a value resolved from a producer's outputs (CALL.x, self.x) is filtered
//     as StructType.FilterJson does (types.NarrowValue): unchanged, or rebuilt
//     in declared order;
//   - an object or array built from bindings (resolveMap, resolveArray) is
//     never filtered itself: mrp builds it with its members' keys sorted, and
//     filters each member where that member resolves;
//   - a whole-call ref (LazyArgumentMap.filter) keeps the struct's declared
//     fields, each filtered, with their keys sorted.
//
// args holds the resolved values decoded, with params their receiving params
// and spec their bindings. Args without a param pass through; args is not
// mutated.
func Narrow(spec Spec, args map[string]any, params []ir.Param, tbl *types.Table) map[string]any {
	out := make(map[string]any, len(args))
	maps.Copy(out, args)

	for _, p := range params {
		if v, ok := args[p.Name]; ok {
			out[p.Name] = narrowEntry(spec[p.Name], v, p, tbl)
		}
	}

	return out
}

// ForkSpec returns the spec binding one fork's args: each split binding becomes
// the binding of the fork's element, the element at index of an array literal
// or key of a map literal, a per-fork struct of a whole ref to a mapped call,
// or otherwise an element of a resolved value. An element sliced from a
// bundle's field has a ref split binding, whose element needs no index (-1).
func ForkSpec(spec Spec, index int, key string) Spec {
	out := make(Spec, len(spec))

	for param, e := range spec {
		if e.Split {
			e = e.element(index, key)
		}

		out[param] = e
	}

	return out
}

func (e Entry) element(index int, key string) Entry {
	switch {
	case e.Array != nil && index >= 0 && index < len(e.Array):
		return e.Array[index]
	case e.Object != nil:
		if el, ok := e.Object[key]; ok {
			return el
		}
	case e.Ref != nil && e.Ref.Fork != "":
		ref := *e.Ref
		ref.Fork = ""

		return Entry{Ref: &ref}
	}

	return Entry{}
}

func narrowEntry(e Entry, v any, p ir.Param, tbl *types.Table) any {
	switch {
	case e.Object != nil:
		return narrowBuilt(e.Object, v, p, tbl)
	case e.Array != nil:
		return narrowArray(e.Array, v, p, tbl)
	case e.Ref != nil && e.Ref.Kind == ir.RefKindCall && e.Ref.Output == "":
		return narrowWholeRef(e.Ref.Fork, v, p, tbl)
	default:
		return tbl.NarrowValue(v, p)
	}
}

// narrowBuilt narrows each member of an object built from bindings by its own
// binding, keeping every member and the object's order.
func narrowBuilt(members map[string]Entry, v any, p ir.Param, tbl *types.Table) any {
	o, ok := types.AsObject(v)
	if !ok {
		return tbl.NarrowValue(v, p)
	}

	out := slices.Clone(o)

	for i, m := range o {
		if mp, ok := memberParam(p, m.Key, tbl); ok {
			out[i].Value = narrowEntry(members[m.Key], m.Value, mp, tbl)
		}
	}

	return out
}

// memberParam returns the type of member key of an object of type p: a typed
// map's element (one map level carrying MapDim-1 inner array dims) or a
// struct's field.
func memberParam(p ir.Param, key string, tbl *types.Table) (ir.Param, bool) {
	if p.MapDim > 0 {
		return elemOfMap(p), true
	}

	fields, ok := tbl.Fields(p.BaseType)
	if !ok || p.ArrayDim > 0 {
		return ir.Param{}, false
	}

	i := slices.IndexFunc(fields, func(f ir.Param) bool { return f.Name == key })
	if i < 0 {
		return ir.Param{}, false
	}

	return fields[i], true
}

func elemOfMap(p ir.Param) ir.Param {
	elem := p
	elem.ArrayDim = p.ArrayDim + p.MapDim - 1
	elem.MapDim = 0

	return elem
}

// narrowArray narrows each element of an array built from bindings by its own
// binding.
func narrowArray(elems []Entry, v any, p ir.Param, tbl *types.Table) any {
	arr, ok := v.([]any)
	if !ok || p.ArrayDim == 0 || len(arr) != len(elems) {
		return tbl.NarrowValue(v, p)
	}

	elem := p
	elem.ArrayDim--

	out := make([]any, len(arr))
	for i, e := range arr {
		out[i] = narrowEntry(elems[i], e, elem, tbl)
	}

	return out
}

// narrowWholeRef filters a whole-call ref's value: the call's outputs, or for a
// mapped call one outputs struct per fork, in an array or keyed by fork.
func narrowWholeRef(fork string, v any, p ir.Param, tbl *types.Table) any {
	switch fork {
	case ForkArray:
		arr, ok := v.([]any)
		if !ok || p.ArrayDim == 0 {
			return tbl.NarrowValue(v, p)
		}

		elem := p
		elem.ArrayDim--

		out := make([]any, len(arr))
		for i, e := range arr {
			out[i] = filterFields(e, elem, tbl)
		}

		return out
	case ForkMap:
		o, ok := types.AsObject(v)
		if !ok || p.MapDim == 0 {
			return tbl.NarrowValue(v, p)
		}

		out := slices.Clone(o)
		for i, m := range o {
			out[i].Value = filterFields(m.Value, elemOfMap(p), tbl)
		}

		return out
	default:
		return filterFields(v, p, tbl)
	}
}

// filterFields keeps a struct's declared fields, each filtered, in a map, so
// they encode with their keys sorted (LazyArgumentMap.filter builds a
// MarshalerMap).
func filterFields(v any, p ir.Param, tbl *types.Table) any {
	o, ok := types.AsObject(v)
	fields, isStruct := tbl.Fields(p.BaseType)

	if !ok || !isStruct || p.ArrayDim > 0 || p.MapDim > 0 {
		return tbl.NarrowValue(v, p)
	}

	out := make(map[string]any, len(fields))

	for _, f := range fields {
		fv, _ := o.Get(f.Name)
		out[f.Name] = tbl.NarrowValue(fv, f)
	}

	return out
}
