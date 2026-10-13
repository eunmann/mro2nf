package cg

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"

	"github.com/martian-lang/martian/martian/core"
	"github.com/martian-lang/martian/martian/syntax"
)

// resolveSplit resolves a split at the fork part that matches its call, or
// the last part (core/resolve.go:553-631): the element of the collection at
// that part's index. A ref already forked on the call is that one element.
func (r *resolver) resolveSplit(e *syntax.SplitExp, t syntax.Type, fork Fork) (json.Marshaler, error) {
	var innerT syntax.Type

	switch e.Source.CallMode() {
	case syntax.ModeArrayCall:
		innerT = r.g.types.GetArray(t, 1)
	case syntax.ModeMapCall:
		innerT = r.g.types.GetMap(t)
	default:
		return nil, fmt.Errorf("%w: split over a %s source", errShape, e.Source.CallMode().String())
	}

	for i, p := range fork {
		if p.Root.Call() != e.Call && i != len(fork)-1 {
			continue
		}

		if p.Root.Call() == e.Call {
			if v, ok, err := r.stagedElement(e, t, fork, p); ok || err != nil {
				return v, err
			}
		}

		return r.splitElement(e, innerT, fork, p.Index)
	}

	return nil, fmt.Errorf("%w: split %s outside its fork", errShape, e.GoString())
}

// splitElement is the element at ix of e's collection, of type innerT, at
// fork. A ref already forked on the call is that one element.
func (r *resolver) splitElement(e *syntax.SplitExp, innerT syntax.Type, fork Fork, ix syntax.CollectionIndex) (json.Marshaler, error) {
	ref, isRef := e.Value.(*syntax.RefExp)
	if isRef {
		if _, forked := ref.Forks[e.Call]; !forked {
			c, err := r.refIndexed(ref, innerT, fork, ix.Mode())
			if err != nil {
				return nil, fmt.Errorf("split %s: %w", e.Source.CallMode().String(), err)
			}

			return c.at(ix)
		}
	}

	v, err := r.resolve(e.Value, innerT, fork)
	if err != nil {
		return v, fmt.Errorf("split %s: %w", e.Source.CallMode().String(), err)
	}

	if isRef {
		return v, nil
	}

	return element(v, ix)
}

// indexedKey names a collection read from a stage output: the output,
// the upstream fork it is read at, the type it is filtered to and the mode
// it forks by.
type indexedKey struct {
	node, output, fork string
	t                  syntax.TypeId
	mode               syntax.CallMode
}

// refIndexed is ref's collection, filtered to t, at fork, as resolve
// reads it, decoded once per resolver: a post resolving a binding at every
// fork of a root reads the same output at each.
func (r *resolver) refIndexed(ref *syntax.RefExp, t syntax.Type, fork Fork, mode syntax.CallMode) (indexed, error) {
	exp, t, err := r.specialize(ref, t, fork)
	if err != nil {
		return indexed{}, err
	}

	sref, ok := exp.(*syntax.RefExp)
	if !ok {
		return indexed{}, fmt.Errorf("%w: %s specializes to %T", errShape, ref.GoString(), exp)
	}

	up, err := r.g.Stage(sref.Id)
	if err != nil {
		return indexed{}, fmt.Errorf("ref %s.%s: %w", sref.Id, sref.OutputId, err)
	}

	uf, err := fork.project(up.ForkRoots(), sref.Forks)
	if err != nil {
		return indexed{}, fmt.Errorf("ref %s.%s: %w", sref.Id, sref.OutputId, err)
	}

	k := indexedKey{node: sref.Id, output: sref.OutputId, fork: uf.key(), t: t.TypeId(), mode: mode}
	if c, ok := r.decoded[k]; ok {
		return c, nil
	}

	v, err := r.resolveRef(sref, t, fork)
	if err != nil {
		return indexed{}, err
	}

	c, err := indexedOf(v, mode)
	if err != nil {
		return indexed{}, err
	}

	if r.decoded == nil {
		r.decoded = map[indexedKey]indexed{}
	}

	r.decoded[k] = c

	return c, nil
}

// stagedElement serves a split from an element a stage's post wrote (Post),
// so an instance never reads the whole collection: the root's fork element
// when the split reads the root's own source, else the partner element of
// the output it reads (partner). It filters the element to t as Path
// filters each element of the collection to t's array or map (syntax
// ArrayType.FilterJson and TypedMapType.FilterJson filter element-wise).
func (r *resolver) stagedElement(e *syntax.SplitExp, t syntax.Type, fork Fork, p Part) (json.Marshaler, bool, error) {
	ref, gate, ok := partnerSource(e)
	if !ok {
		return nil, false, nil
	}

	if !r.g.ownSource(p.Root, ref) {
		return r.partnerElement(ref, gate, t, fork, p)
	}

	outer, ok := fork.restrict(r.g.outerRoots(p.Root))
	if !ok || gate != nil {
		return nil, false, nil
	}

	src, err := r.sourceExp(p.Root, outer)
	if err != nil {
		return nil, false, err
	}

	if sref, ok := src.Value.(*syntax.RefExp); !ok || !sameRef(ref, sref) {
		return nil, false, nil
	}

	raw, ok, err := r.st.element(p.Root, outer.with(p))
	if err != nil || !ok {
		return nil, ok, err
	}

	return r.filterElement(raw, t, p)
}

// partnerElement serves a split of ref's output from its partner element
// at p, read through gate, when set: where the gate disables ref's stage,
// the split resolves as the gated value, which reads null.
func (r *resolver) partnerElement(ref, gate *syntax.RefExp, t syntax.Type, fork Fork, p Part) (json.Marshaler, bool, error) {
	if gate != nil {
		if off, err := r.disabledBy(gate, fork); err != nil || off {
			return nil, false, err
		}
	}

	raw, ok, err := r.st.partner(p.Root, ref, p.Index)
	if err != nil || !ok || raw == nil {
		return nil, ok, err
	}

	return r.filterElement(raw, t, p)
}

// filterElement filters a staged element of p's root to t.
func (r *resolver) filterElement(raw json.RawMessage, t syntax.Type, p Part) (json.Marshaler, bool, error) {
	v, fatal, err := t.FilterJson(raw, r.g.types)
	if err != nil && fatal {
		return nil, false, fmt.Errorf("filter fork element of %s to %s: %w", p.Root.Fqid, t, err)
	}

	return v, true, nil
}

func sameRef(a, b *syntax.RefExp) bool {
	if a.Id != b.Id || a.OutputId != b.OutputId || len(a.Forks) != len(b.Forks) {
		return false
	}

	for c, ai := range a.Forks {
		bi, ok := b.Forks[c]
		if !ok || (ai == nil) != (bi == nil) || (ai != nil && !indexEqual(ai, bi)) {
			return false
		}
	}

	return true
}

// element takes the element of a resolved collection at ix.
func element(v json.Marshaler, ix syntax.CollectionIndex) (json.Marshaler, error) {
	c, err := indexedOf(v, ix.Mode())
	if err != nil {
		return nil, err
	}

	return c.at(ix)
}

// indexed is a resolved collection decoded once, so that taking the element
// of each of its N forks costs O(1), not a decode of the whole: a post or a
// gate enumerating every fork would otherwise be O(N²).
type indexed struct {
	mode syntax.CallMode
	null bool
	arr  []json.RawMessage
	m    map[string]json.RawMessage
}

// indexedOf decodes v as the collection a split of mode forks over: an
// array, or a map for a map call.
func indexedOf(v json.Marshaler, mode syntax.CallMode) (indexed, error) {
	raw, err := marshal(v)
	if err != nil {
		return indexed{}, err
	}

	c := indexed{mode: mode, null: isNull(raw)}

	switch {
	case c.null:
	case mode == syntax.ModeMapCall:
		if err := json.Unmarshal(raw, &c.m); err != nil {
			return indexed{}, fmt.Errorf("decode map collection: %w", err)
		}
	default:
		if err := json.Unmarshal(raw, &c.arr); err != nil {
			return indexed{}, fmt.Errorf("decode array collection: %w", err)
		}
	}

	return c, nil
}

// indices are the collection's fork indices in mrp's order; null and empty
// have none (core/fork.go:1639-1790 expandForkFromObj).
func (c indexed) indices() []syntax.CollectionIndex {
	switch {
	case c.null:
		return nil
	case c.mode == syntax.ModeMapCall:
		return mapIndices(slices.Collect(maps.Keys(c.m)))
	default:
		return arrayIndices(len(c.arr))
	}
}

// at is the element at ix (core/resolve.go getElement, getArrayElement,
// getMapElement): null in a null collection, and null with errNoElement past
// an array's end or for a key a map lacks.
func (c indexed) at(ix syntax.CollectionIndex) (json.Marshaler, error) {
	switch {
	case c.null:
		return nil, nil
	case ix.Mode() != c.mode:
		return nil, fmt.Errorf("%w: %s index into a %s collection", errShape, ix.Mode().String(), c.mode.String())
	case c.mode == syntax.ModeMapCall:
		el, ok := c.m[ix.MapKey()]
		if !ok {
			return nil, fmt.Errorf("%w: map key %q not found", errNoElement, ix.MapKey())
		}

		return el, nil
	case ix.ArrayIndex() < 0 || ix.ArrayIndex() >= len(c.arr):
		return nil, fmt.Errorf("%w: array index %d >= %d", errNoElement, ix.ArrayIndex(), len(c.arr))
	default:
		return c.arr[ix.ArrayIndex()], nil
	}
}

// resolveMerge gathers a value over every fork of a map call
// (core/resolve.go:292-440): inside the call's own fork it is that fork's
// value; outside it is a map by key or an array by index over the call's
// forks, the typed empty when it has none, and null over a null source.
func (r *resolver) resolveMerge(e *syntax.MergeExp, t syntax.Type, fork Fork) (json.Marshaler, error) {
	innerT, err := mergeElemType(e, t)
	if err != nil {
		return nil, err
	}

	if _, ok := fork.part(e.GetCall()); ok {
		return r.resolve(e.Value, innerT, fork)
	}

	if e.CallMode() == syntax.ModeNullMapCall {
		return nil, nil
	}

	parts, err := r.mergeParts(e, fork)
	if err != nil {
		return nil, fmt.Errorf("merge of %s: %w", e.Call.Fqid, err)
	}

	if e.CallMode() == syntax.ModeMapCall {
		out := make(core.MarshalerMap, len(parts.list))

		for _, ix := range parts.list {
			v, err := r.resolve(e.Value, innerT, fork.with(Part{Root: e.Call, Index: ix}))
			if err != nil {
				return nil, fmt.Errorf("merge of %s key %q: %w", e.Call.Fqid, ix.MapKey(), err)
			}

			out[ix.MapKey()] = v
		}

		return out, nil
	}

	out := make(marshalerArray, len(parts.list))

	for i, ix := range parts.list {
		v, err := r.mergeElement(e, innerT, fork, parts, ix)
		if err != nil {
			return nil, fmt.Errorf("merge of %s index %d: %w", e.Call.Fqid, i, err)
		}

		out[i] = v
	}

	return out, nil
}

// mergeElemType is the type of one merged value (core/resolve.go:294-322).
func mergeElemType(e *syntax.MergeExp, t syntax.Type) (syntax.Type, error) {
	switch mt := t.(type) {
	case *syntax.ArrayType:
		if e.MergeOver.CallMode() != syntax.ModeArrayCall {
			return nil, fmt.Errorf("%w: array merge over a %s mapping", errShape, e.MergeOver.CallMode().String())
		}

		if mt.Dim > 1 {
			nt := *mt
			nt.Dim--

			return &nt, nil
		}

		return mt.Elem, nil
	case *syntax.TypedMapType:
		if e.MergeOver.CallMode() != syntax.ModeMapCall {
			return nil, fmt.Errorf("%w: map merge over a %s mapping", errShape, e.MergeOver.CallMode().String())
		}

		return mt.Elem, nil
	default:
		return nil, fmt.Errorf("%w: merge of %s into %s", errShape, e.Call.Fqid, t)
	}
}

// indices returns root's fork indices under outer in mrp's order. A source
// whose values are at hand is evaluated as core expands a fork
// (core/fork.go:1389-1790); otherwise the indices are those of the staged
// instances forked on root, which a gathering task stages in full.
func (r *resolver) indices(root *syntax.CallGraphStage, outer Fork) ([]syntax.CollectionIndex, error) {
	idx, err := r.sourceIndices(root, outer)
	if errors.Is(err, ErrNotStaged) {
		return r.st.instances(root, outer), nil
	}

	return idx, err
}

func (r *resolver) sourceIndices(root *syntax.CallGraphStage, outer Fork) ([]syntax.CollectionIndex, error) {
	sp := root.Split()
	if sp == nil {
		return nil, fmt.Errorf("indices of %s: %w: not a map call", root.Fqid, errShape)
	}

	if idx, ok := knownIndices(sp.Source); ok {
		return idx, nil
	}

	v, err := r.sourceValue(root, outer)
	if err != nil {
		return nil, fmt.Errorf("indices of %s: %w", root.Fqid, err)
	}

	c, err := indexedOf(v, sp.Source.CallMode())

	return c.indices(), err
}

// sourceExp specializes root's split for outer, as core Fork.expandForkPart
// does before it expands a fork from the split's source (core/fork.go:1389).
// A split the specialization removes has a null source.
func (r *resolver) sourceExp(root *syntax.CallGraphStage, outer Fork) (*syntax.SplitExp, error) {
	sp := root.Split()

	exp, err := sp.BindingPath("", outer.indexMap(), r.g.types)
	if err != nil {
		return nil, fmt.Errorf("specialize source of %s: %w", root.Fqid, err)
	}

	s, ok := exp.(*syntax.SplitExp)
	if !ok {
		return &syntax.SplitExp{Value: &syntax.NullExp{}, Type: sp.Type, Call: sp.Call, Source: sp.Source}, nil
	}

	return s, nil
}

// sourceValue is root's split collection under outer. A ref source is read
// with its declared type, as core expandForkFromRef reads it
// (core/fork.go:1625).
func (r *resolver) sourceValue(root *syntax.CallGraphStage, outer Fork) (json.Marshaler, error) {
	s, err := r.sourceExp(root, outer)
	if err != nil {
		return nil, err
	}

	if ref, ok := s.Value.(*syntax.RefExp); ok {
		return r.resolveRef(ref, nil, outer)
	}

	return r.resolve(s.Value, root.Split().Type, outer)
}

// knownIndices are the indices of a source of compile-time length: array
// positions, or map keys in byte order (core/resolve.go:474-513).
func knownIndices(src syntax.MapCallSource) ([]syntax.CollectionIndex, bool) {
	if !src.KnownLength() {
		return nil, false
	}

	switch src.CallMode() {
	case syntax.ModeArrayCall:
		return arrayIndices(src.ArrayLength()), true
	case syntax.ModeMapCall:
		keys := make([]string, 0, len(src.Keys()))
		for k := range src.Keys() {
			keys = append(keys, k)
		}

		return mapIndices(keys), true
	default:
		return nil, true
	}
}

func arrayIndices(n int) []syntax.CollectionIndex {
	idx := make([]syntax.CollectionIndex, n)
	for i := range idx {
		idx[i] = ArrayIndex(i)
	}

	return idx
}

func mapIndices(keys []string) []syntax.CollectionIndex {
	sort.Strings(keys)

	idx := make([]syntax.CollectionIndex, len(keys))
	for i, k := range keys {
		idx[i] = MapKey(k)
	}

	return idx
}
