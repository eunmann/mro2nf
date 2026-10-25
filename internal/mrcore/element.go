package mrcore

import (
	"encoding/json"
	"fmt"

	"github.com/martian-lang/martian/martian/syntax"
)

const disabledBinding = "disabled binding"

// resolved is what core's resolvers return: whether the reads were ready,
// the value, and the error.
type resolved struct {
	ready bool
	v     json.Marshaler
	err   error
}

// splitElement resolves the element of a split at part without resolving
// the collection, as core's resolveSplit would and then index it
// (resolve.go:604): an instance is given only its own element's upstream
// outs. It is the element the task staged (Store.SplitElement), the element
// of a stage output's collection decoded once, or one element of a literal
// collection within any gates; false where none applies.
func (top *TopNode) splitElement(binding *syntax.SplitExp, t syntax.Type,
	fork ForkId, part *ForkSourcePart, readSize int64,
) (resolved, bool) {
	if part.Id.IndexSource() != nil {
		return resolved{}, false
	}

	if part.Split.Call == binding.Call {
		v, ok, err := top.store.SplitElement(binding, t, fork, part)
		if err != nil {
			return resolved{ready: true, err: fmt.Errorf("staged element of split %s: %w", binding.Call.Id, err)}, true
		}

		if ok {
			return resolved{ready: true, v: v}, true
		}
	}

	if ref, ok := binding.Value.(*syntax.RefExp); ok {
		return top.refElement(binding, ref, t, fork, part, readSize)
	}

	return top.literalElement(binding.Value, t, fork, part.Id, readSize)
}

// collectionKey names a collection a split reads from a stage output: the
// upstream fork, the output, and the type it is filtered to.
type collectionKey struct {
	fork, node, output string
	t                  syntax.TypeId
}

// refElement takes the element at part of the collection a split reads
// from ref, the collection resolved and decoded once per upstream fork,
// output and type: a post resolving a binding at each of N forks would
// otherwise decode it N times. A ref already forked on the split's call
// is that element, and a collection that does not resolve fails in core's
// own resolveSplit, which both leave to it.
func (top *TopNode) refElement(binding *syntax.SplitExp, ref *syntax.RefExp, t syntax.Type,
	fork ForkId, part *ForkSourcePart, readSize int64,
) (resolved, bool) {
	if _, forked := ref.Forks[binding.Call]; forked {
		return resolved{}, false
	}

	c, ok := top.collection(binding, ref, t, fork, readSize)
	if !ok {
		return resolved{}, false
	}

	e, err := getElement(c, part.Id)
	if raw, ok := e.(json.RawMessage); ok && raw == nil {
		// getMapElement returns a missing key's zero RawMessage, which
		// MarshalerMap writes as no bytes; the element reads null.
		e = nil
	}

	if err != nil {
		err = &elementError{
			element: "splitting " + binding.Source.CallMode().String() +
				" " + binding.Value.GoString() + " with fork part " +
				part.Split.Call.GoString(),
			inner: err,
		}
	}

	return resolved{ready: true, v: e, err: err}, true
}

// collection is the collection binding splits, read from ref at fork and
// decoded, once per upstream fork, output and type; false where it does not
// resolve.
func (top *TopNode) collection(binding *syntax.SplitExp, ref *syntax.RefExp, t syntax.Type,
	fork ForkId, readSize int64,
) (json.Marshaler, bool) {
	bound := top.allNodes[ref.Id]
	if bound == nil {
		return nil, false
	}

	f, err := bound.matchFork(ref.Forks, fork)
	if err != nil {
		return nil, false
	}

	innerT := splitType(top.types, binding, t)
	k := collectionKey{fork: f.fqname, node: ref.Id, output: ref.OutputId, t: innerT.TypeId()}

	if c, ok := top.collections[k]; ok {
		return c, true
	}

	ready, v, err := f.resolveRef(ref, innerT, bound.call.Call().DecId, readSize)
	if err != nil || !ready {
		return nil, false
	}

	c, ok := decodeCollection(v, binding.Source.CallMode())
	if !ok {
		return nil, false
	}

	if top.collections == nil {
		top.collections = map[collectionKey]json.Marshaler{}
	}

	top.collections[k] = c

	return c, true
}

// splitType is the type of the collection binding splits into elements of
// type t, as resolveSplit types it.
func splitType(types *syntax.TypeLookup, binding *syntax.SplitExp, t syntax.Type) syntax.Type {
	if binding.Source.CallMode() == syntax.ModeMapCall {
		return types.GetMap(t)
	}

	return types.GetArray(t, 1)
}

// decodeCollection decodes raw JSON v once into the collection getElement
// indexes in O(1); false where it does not decode.
func decodeCollection(v json.Marshaler, mode syntax.CallMode) (json.Marshaler, bool) {
	raw, ok := v.(json.RawMessage)
	if !ok {
		return v, true
	}

	if mode == syntax.ModeMapCall {
		var m LazyArgumentMap
		if json.Unmarshal(raw, &m) != nil {
			return nil, false
		}

		return m, true
	}

	var a marshallerArray
	if a.UnmarshalJSON(raw) != nil {
		return nil, false
	}

	return a, true
}

// literalElement resolves the element at ix of a literal collection v, null
// where a gate around it disables it; false where v is no literal.
func (top *TopNode) literalElement(v syntax.Exp, t syntax.Type, fork ForkId,
	ix syntax.CollectionIndex, readSize int64,
) (resolved, bool) {
	switch x := v.(type) {
	case *syntax.ArrayExp, *syntax.MapExp:
		el, err := getElement(x, ix)
		if err != nil {
			return resolved{ready: true, err: err}, true
		}

		e, ok := el.(syntax.Exp)
		if !ok {
			return resolved{ready: true, v: el}, true
		}

		ready, out, err := top.resolve(e, t, fork, readSize)

		return resolved{ready: ready, v: out, err: err}, true
	case *syntax.DisabledExp:
		ready, off, err := top.gate(x, fork, readSize)
		if err != nil || !ready || off {
			return resolved{ready: ready, err: err}, true
		}

		return top.literalElement(x.Value, t, fork, ix, readSize)
	default:
		return resolved{}, false
	}
}

// gate resolves a disable-wrapped value's gate as resolveDisabledExp does
// (resolve.go:248): whether its reads were ready, and whether it disables
// the value.
func (top *TopNode) gate(d *syntax.DisabledExp, fork ForkId, readSize int64) (bool, bool, error) {
	ready, v, err := top.resolve(d.Disabled,
		top.types.Get(syntax.TypeId{Tname: syntax.KindBool}), fork, readSize)
	if err != nil || !ready {
		return ready, false, err
	}

	switch g := v.(type) {
	case *syntax.BoolExp:
		return true, g.Value, nil
	case json.RawMessage:
		var b bool
		if err := json.Unmarshal(g, &b); err != nil {
			return true, false, &elementError{element: disabledBinding, inner: err}
		}

		return true, b, nil
	default:
		return true, false, &elementError{
			element: fmt.Sprintf("invalid type %T for %s from %s", v, disabledBinding, d.Disabled.GoString()),
		}
	}
}
