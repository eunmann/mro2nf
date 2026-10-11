package gen

import "strings"

// Type is a Martian type id, the shape syntax.TypeId gives every type: a base
// name under Arr array dimensions, where a Map > 0 type is map<Base> with
// Map-1 array dimensions inside the map. Martian has no map<map<T>>, so one
// map level is all a type can hold.
type Type struct {
	Base string `json:"base"`
	Arr  int    `json:"arr,omitempty"`
	Map  int    `json:"map,omitempty"`
}

// String renders t as MRO source: map<Base[]...>[]...
func (t Type) String() string {
	dims := strings.Repeat("[]", t.Arr)
	if t.Map > 0 {
		return "map<" + t.Base + strings.Repeat("[]", t.Map-1) + ">" + dims
	}

	return t.Base + dims
}

// Scalar reports whether t has no collection dimension.
func (t Type) Scalar() bool { return t.Arr == 0 && t.Map == 0 }

// IsArray reports whether a split over t forks per array element.
func (t Type) IsArray() bool { return t.Arr > 0 }

// IsMap reports whether a split over t forks per map key.
func (t Type) IsMap() bool { return t.Arr == 0 && t.Map > 0 }

// Elem is the type of one element of a collection: what a split over t binds
// each fork.
func (t Type) Elem() Type {
	if t.Arr > 0 {
		return Type{Base: t.Base, Arr: t.Arr - 1, Map: t.Map}
	}

	return Type{Base: t.Base, Arr: t.Map - 1}
}

// ArrayOf is t[].
func (t Type) ArrayOf() Type { return Type{Base: t.Base, Arr: t.Arr + 1, Map: t.Map} }

// MapOf is map<t>, or false when t is already a map.
func (t Type) MapOf() (Type, bool) {
	if t.Map > 0 {
		return Type{}, false
	}

	return Type{Base: t.Base, Map: t.Arr + 1}, true
}

// lift is the type a map call in the given fork mode gives an output of type
// t, or false for a map fork over an output that is already a map.
func (t Type) lift(mode forkMode) (Type, bool) {
	switch mode {
	case forkArray:
		return t.ArrayOf(), true
	case forkMap:
		return t.MapOf()
	default:
		return t, true
	}
}

// project is the type of field f (of type ft) read through a value of type t
// whose base is a struct: the field's type under t's dimensions. It is false
// for the shapes mro2nf rejects at transpile time (a field read through a
// map<S[]>) and for one that would nest a map in a map.
func (t Type) project(ft Type) (Type, bool) {
	switch {
	case t.Map > 1:
		return Type{}, false
	case t.Map == 1 && ft.Map > 0:
		return Type{}, false
	case t.Map == 1:
		return Type{Base: ft.Base, Arr: t.Arr, Map: 1 + ft.Arr}, true
	default:
		return Type{Base: ft.Base, Arr: t.Arr + ft.Arr, Map: ft.Map}, true
	}
}

// forkMode is how a map call forks: not at all, per array element or per key.
type forkMode int

const (
	forkNone forkMode = iota
	forkArray
	forkMap
)

func modeOf(t Type) forkMode {
	if t.IsArray() {
		return forkArray
	}

	return forkMap
}

const (
	typeInt    = "int"
	typeFloat  = "float"
	typeString = "string"
	typeBool   = "bool"
	typeFile   = "file"
	typePath   = "path"
)
