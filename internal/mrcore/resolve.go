//
// Copyright (c) 2020 10X Genomics, Inc. All rights reserved.
//
// Methods for resolving bindings at runtime to concrete values.
//

package mrcore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/martian-lang/martian/martian/syntax"
)

// resolve will either return true and the concrete value represented by the
// expression, false if some of the referred-to nodes are still running, or
// an error if there was a problem resolving a reference.
func (node *TopNode) resolve(binding syntax.Exp, t syntax.Type,
	fork ForkId,
	readSize int64) (bool, json.Marshaler, error) {
	if binding == nil {
		return true, nil, nil
	}
	if !binding.HasRef() && !binding.HasSplit() {
		return true, binding, nil
	}
	if len(fork) > 0 {
		br := syntax.ResolvedBinding{
			Exp:  binding,
			Type: t,
		}
		if b, err := br.BindingPath("", fork.SourceIndexMap(), node.Types()); err != nil {
			return true, nil, &elementError{
				element: "resolving binding",
				inner:   err,
			}
		} else if binding != b.Exp {
			binding = b.Exp
			if !binding.HasRef() && !binding.HasSplit() {
				return true, binding, nil
			}
			t = b.Type
		}
	}
	switch binding := binding.(type) {
	case *syntax.RefExp:
		return node.resolveRef(binding, t, fork, readSize)
	case *syntax.ArrayExp:
		return node.resolveArray(binding, t, fork, readSize)
	case *syntax.MapExp:
		return node.resolveMap(binding, t, fork, readSize)
	case *syntax.SplitExp:
		return node.resolveSplit(binding, t, fork, readSize)
	case *syntax.MergeExp:
		return node.resolveMerge(binding, t, fork, readSize)
	case *syntax.DisabledExp:
		return node.resolveDisabledExp(binding, t, fork, readSize)
	default:
		tid := t.TypeId()
		panic(fmt.Sprintf("unexpected ref or sweep type %T, wanted %s",
			binding, tid.String()))
	}
}

func (node *TopNode) resolveDisabledExp(binding *syntax.DisabledExp, t syntax.Type,
	fork ForkId, readSize int64) (bool, json.Marshaler, error) {
	ready, disabled, err := node.resolve(binding.Disabled,
		node.Types().Get(syntax.TypeId{Tname: syntax.KindBool}), fork, readSize)
	if err != nil {
		return ready, nil, &elementError{
			element: "disabled binding",
			inner:   err,
		}
	} else if !ready {
		return ready, nil, nil
	}
	switch disabled := disabled.(type) {
	case *syntax.BoolExp:
		if disabled.Value {
			return true, nil, nil
		}
	case json.RawMessage:
		var db bool
		if err := json.Unmarshal(disabled, &db); err != nil {
			return true, nil, &elementError{
				element: "disabled binding",
				inner:   err,
			}
		}
		if db {
			return true, nil, nil
		}
	default:
		return true, nil, &elementError{
			element: fmt.Sprintf(
				"invalid type %T for disabled binding from %s",
				disabled, binding.Disabled.GoString()),
		}
	}
	ready, result, err := node.resolve(binding.Value, t, fork, readSize)
	if err != nil {
		return ready, result, &elementError{
			element: "enabled value",
			inner:   err,
		}
	}
	return ready, result, err
}

// constrain returns a fork ID which includes the statically-known indices from
// ref for any of the given roots which are not already part of this fork ID,
// ordered as in roots.  If there are no such indices, it returns this fork ID.
func (f ForkId) constrain(ref map[*syntax.CallStm]syntax.CollectionIndex,
	roots []*syntax.CallStm) ForkId {
	staticIndex := func(src *syntax.CallStm) syntax.CollectionIndex {
		if _, err := f.matchPart(src); err == nil {
			return nil
		}
		if j := ref[src]; j != nil && j.IndexSource() == nil {
			return j
		}
		return nil
	}
	if !slices.ContainsFunc(roots, func(src *syntax.CallStm) bool {
		return staticIndex(src) != nil
	}) {
		return f
	}
	result := make(ForkId, 0, len(roots))
	for _, src := range roots {
		if p, err := f.matchPart(src); err == nil {
			result = append(result, p)
		} else if j := staticIndex(src); j != nil {
			result = append(result, &ForkSourcePart{
				Id: convertForkPart(j),
				Split: &syntax.SplitExp{
					Value:  &syntax.MergeExp{MergeOver: src},
					Call:   src,
					Source: src,
				},
			})
		}
	}
	return result
}

func getElement(result json.Marshaler,
	element syntax.CollectionIndex) (json.Marshaler, error) {
	switch element.Mode() {
	case syntax.ModeArrayCall:
		if element.IndexSource() != nil {
			m, err := getArrayElement(result, 0)
			return m, &elementError{
				element: "unknown index",
				inner:   err,
			}
		}
		return getArrayElement(result, element.ArrayIndex())
	case syntax.ModeMapCall:
		if element.IndexSource() != nil {
			m, err := getMapElement(result, "")
			return m, &elementError{
				element: "unknown key",
				inner:   err,
			}
		}
		return getMapElement(result, element.MapKey())
	default:
		if element.IndexSource() != nil {
			return nil, &forkResolutionError{
				Msg: "unresolved index for map call of unknown type",
			}
		}
		return nil, &elementError{
			element: "unknown element type " + element.Mode().String() +
				" (" + element.GoString() + ")",
		}
	}
}

func getMapElement(m json.Marshaler, k string) (json.Marshaler, error) {
	switch m := m.(type) {
	case *syntax.NullExp:
		return m, nil
	case json.RawMessage:
		var r LazyArgumentMap
		if err := json.Unmarshal(m, &r); err != nil {
			return m, &elementError{
				element: "map key " + k,
				inner:   err,
			}
		}
		v, ok := r[k]
		if !ok {
			return v, &elementError{
				element: "map key " + k + " not found",
				inner:   missingValueError,
			}
		}
		return v, nil
	case LazyArgumentMap:
		v, ok := m[k]
		if !ok {
			return v, &elementError{
				element: "map key " + k + " not found",
				inner:   missingValueError,
			}
		}
		return v, nil
	case MarshalerMap:
		v, ok := m[k]
		if !ok {
			return v, &elementError{
				element: "map key " + k + " not found",
				inner:   missingValueError,
			}
		}
		return v, nil
	case *syntax.MapExp:
		v, ok := m.Value[k]
		if !ok {
			return v, &elementError{
				element: "map key " + k + " not found",
				inner:   missingValueError,
			}
		}
		return v, nil
	default:
		return nil, &syntax.IncompatibleTypeError{
			Message: fmt.Sprintf("can't take map element from %T", m),
		}
	}
}

func (node *TopNode) resolveRef(binding *syntax.RefExp, t syntax.Type,
	fork ForkId,
	readSize int64) (bool, json.Marshaler, error) {
	boundNode := node.allNodes[binding.Id]
	if boundNode == nil {
		panic("unknown bound node - this should not be possible in properly-compiled code")
	}
	return boundNode.resolveRef(binding, t, fork, readSize)
}

func (boundNode *Node) resolveRef(binding *syntax.RefExp, t syntax.Type,
	fork ForkId,
	readSize int64) (bool, json.Marshaler, error) {
	f, err := boundNode.matchFork(binding.Forks, fork)
	if err != nil {
		return true, nil, &forkResolutionError{
			Msg: "could not match reference to a specific fork of " +
				boundNode.GetFQName(),
			Err: err,
		}
	}
	return f.resolveRef(binding, t,
		boundNode.call.Call().DecId, readSize)
}

type elementError struct {
	inner   error
	element string
}

func (err *elementError) Error() string {
	if err.inner == nil {
		return err.element
	}
	suffix := err.inner.Error()
	if strings.IndexRune(suffix, '\n')+len(err.element) > 40 {
		return err.element + ":\n\t" + suffix
	}
	return err.element + ": " + suffix
}

func (err *elementError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.inner
}

type forkResolutionError struct {
	Err error
	Msg string
}

func (err *forkResolutionError) Error() string {
	if err.Err == nil {
		return err.Msg
	}
	suffix := err.Err.Error()
	if strings.IndexRune(suffix, '\n')+len(err.Msg) > 40 {
		return err.Msg + ":\n\t" + suffix
	}
	return err.Msg + ": " + suffix
}

func (err *forkResolutionError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.Err
}

var missingValueError = ErrNoElement

// Path looks up map keys, separated by '.' characters, and projecting through

type marshallerArray []json.Marshaler

func (arr marshallerArray) MarshalJSON() ([]byte, error) {
	if arr == nil {
		return nullBytes[:len(nullBytes):len(nullBytes)], nil
	} else if len(arr) == 0 {
		return []byte(`[]`), nil
	}
	var buf bytes.Buffer
	buf.Grow(2 + len(arr)*5)
	err := arr.encodeJSON(&buf)
	return buf.Bytes(), err
}

func (arr marshallerArray) EncodeJSON(buf *bytes.Buffer) error {
	if arr == nil {
		_, err := buf.Write(nullBytes)
		return err
	} else if len(arr) == 0 {
		_, err := buf.WriteString(`[]`)
		return err
	}
	return arr.encodeJSON(buf)
}

func (arr marshallerArray) encodeJSON(buf *bytes.Buffer) error {
	if _, err := buf.WriteRune('['); err != nil {
		return err
	}
	for i, elem := range arr {
		if i != 0 {
			if _, err := buf.WriteRune(','); err != nil {
				return err
			}
		}
		if elem == nil {
			if _, err := buf.WriteString("null"); err != nil {
				return err
			}
		} else {
			switch elem := elem.(type) {
			case syntax.JsonWriter:
				if err := elem.EncodeJSON(buf); err != nil {
					return err
				}
			case json.RawMessage:
				if _, err := buf.Write(elem); err != nil {
					return err
				}
			default:
				if b, err := elem.MarshalJSON(); err != nil {
					return err
				} else if _, err := buf.Write(b); err != nil {
					return err
				}
			}
		}
	}
	_, err := buf.WriteRune(']')
	return err
}

func (arr *marshallerArray) UnmarshalJSON(b []byte) error {
	if bytes.Equal(b, nullBytes) {
		*arr = nil
		return nil
	}
	var ra []json.RawMessage
	if err := json.Unmarshal(b, &ra); err != nil {
		return err
	}
	*arr = make(marshallerArray, len(ra))
	for i, v := range ra {
		(*arr)[i] = v
	}
	return nil
}

// CallMode Returns the call mode for a call which depends on this source.
func (arr marshallerArray) CallMode() syntax.CallMode {
	return syntax.ModeArrayCall
}

// KnownLength returns true if the source is an array with a known length
// or is a map with a known set of keys.
func (arr marshallerArray) KnownLength() bool {
	return true
}

// If KnownLength is true and CallMode is ModeArrayCall, ArrayLength returns
// the length of the array referred to by this source.  Otherwise it will
// return -1.
func (arr marshallerArray) ArrayLength() int {
	return len(arr)
}

// If KnownLength is true and CallMode is ModeMapCall, MapKeys will return
// a map[string]Exp with the same keys which any call mapping over this
// source would have.  The values are arbitrary.  Otherwise, it will return
// nil.
func (arr marshallerArray) Keys() map[string]syntax.Exp {
	return nil
}

func (arr marshallerArray) GoString() string {
	var buf strings.Builder
	if err := buf.WriteByte('['); err != nil {
		panic(err)
	}
	for i, v := range arr {
		if i != 0 {
			if err := buf.WriteByte(','); err != nil {
				panic(err)
			}
		}
		if _, err := fmt.Fprint(&buf, v); err != nil {
			panic(err)
		}
	}
	if err := buf.WriteByte(']'); err != nil {
		panic(err)
	}
	return buf.String()
}

func (node *TopNode) resolveArray(binding *syntax.ArrayExp, t syntax.Type,
	fork ForkId,
	readSize int64) (bool, json.Marshaler, error) {
	result := make(marshallerArray, len(binding.Value))
	if at, ok := t.(*syntax.ArrayType); !ok {
		id := t.TypeId()
		return true, nil, &syntax.IncompatibleTypeError{
			Message: id.String() + " is not an array type",
		}
	} else {
		t = node.types.GetArray(at.Elem, at.Dim-1)
	}
	allReady := true
	var errs syntax.ErrorList
	for i, exp := range binding.Value {
		if ready, v, err := node.resolve(exp, t,
			fork, readSize); err != nil {
			allReady = ready && allReady
			errs = append(errs, &elementError{
				element: "array index " + strconv.Itoa(i),
				inner:   err,
			})
		} else if !ready {
			allReady = false
		} else {
			result[i] = v
		}
	}
	return allReady, result, errs.If()
}

func (node *TopNode) resolveMap(binding *syntax.MapExp, t syntax.Type,
	fork ForkId,
	readSize int64) (bool, json.Marshaler, error) {
	result := make(MarshalerMap, len(binding.Value))
	allReady := true
	var errs syntax.ErrorList
	switch t := t.(type) {
	case *syntax.TypedMapType:
		for key, exp := range binding.Value {
			if ready, v, err := node.resolve(exp, t.Elem,
				fork, readSize); err != nil {
				allReady = ready && allReady
				errs = append(errs, &elementError{
					element: "key " + key,
					inner:   err,
				})
			} else if !ready {
				allReady = false
				result[key] = nil
			} else {
				result[key] = v
			}
		}
	case *syntax.StructType:
		for _, member := range t.Members {
			if ready, v, err := node.resolve(
				binding.Value[member.Id],
				node.types.Get(member.Tname),
				fork,
				readSize); err != nil {
				allReady = ready && allReady
				errs = append(errs, &elementError{
					element: "field " + member.Id,
					inner:   err,
				})
				result[member.Id] = nil
			} else if !ready {
				allReady = false
				result[member.Id] = nil
			} else {
				result[member.Id] = v
			}
		}
	default:
		return true, nil, &syntax.IncompatibleTypeError{
			Message: fmt.Sprintf("not a map type %T", t),
		}
	}
	return allReady, result, errs.If()
}
