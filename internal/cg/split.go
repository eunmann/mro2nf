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
// the last part (core/resolve.go:604-682): the element of the collection at
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

		return r.splitElement(e, t, innerT, fork, p.Index)
	}

	return nil, fmt.Errorf("%w: split %s outside its fork", errShape, e.GoString())
}

// splitElement is the element at ix, of type t, of e's collection, of type
// innerT, at fork. A ref already forked on the call is that one element. Of
// a literal collection only the element at ix resolves (literalSplitElement).
func (r *resolver) splitElement(e *syntax.SplitExp, t, innerT syntax.Type, fork Fork, ix syntax.CollectionIndex) (json.Marshaler, error) {
	if v, ok, err := r.literalSplitElement(e.Value, t, fork, ix); ok || err != nil {
		if err != nil {
			return v, fmt.Errorf("split %s: %w", e.Source.CallMode().String(), err)
		}

		return v, nil
	}

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

// literalSplitElement resolves the element at ix of v, a literal collection
// within any gates, as v would resolve and then be indexed, without
// resolving its other elements, whose upstream outs an instance is not
// given: null where a gate disables v. It is false for any other v.
func (r *resolver) literalSplitElement(v syntax.Exp, t syntax.Type, fork Fork, ix syntax.CollectionIndex) (json.Marshaler, bool, error) {
	switch x := v.(type) {
	case *syntax.ArrayExp, *syntax.MapExp:
		el, err := literalElement(x, ix)
		if err != nil {
			return nil, true, err
		}

		out, err := r.resolve(el, t, fork)

		return out, true, err
	case *syntax.DisabledExp:
		if off, err := r.disabledBy(x.Disabled, fork); err != nil || off {
			return nil, true, err
		}

		return r.literalSplitElement(x.Value, t, fork, ix)
	default:
		return nil, false, nil
	}
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
		if !ok || !sameIndex(ai, bi) {
			return false
		}
	}

	return true
}

// sameIndex reports whether two references' indices of one call are the
// same: both absent, both concrete and equal, or both left to the fork the
// reference resolves at, over the same source, whose index Martian has not
// resolved and which has no concrete value to read.
func sameIndex(a, b syntax.CollectionIndex) bool {
	switch {
	case a == nil || b == nil:
		return a == nil && b == nil
	case a.IndexSource() != nil || b.IndexSource() != nil:
		return a.IndexSource() == b.IndexSource()
	default:
		return indexEqual(a, b)
	}
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
// have none (core/fork.go:1657-1790 expandForkFromObj).
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
// (core/resolve.go:293-491): inside the call's own fork it is that fork's
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

	list, err := r.mergeIndices(e, fork)
	if err != nil {
		return nil, fmt.Errorf("merge of %s: %w", e.Call.Fqid, err)
	}

	if e.CallMode() == syntax.ModeMapCall {
		out := make(core.MarshalerMap, len(list))

		for _, ix := range list {
			v, err := r.resolve(e.Value, innerT, fork.with(Part{Root: e.Call, Index: ix}))
			if err != nil {
				return nil, fmt.Errorf("merge of %s key %q: %w", e.Call.Fqid, ix.MapKey(), err)
			}

			out[ix.MapKey()] = v
		}

		return out, nil
	}

	out := make(marshalerArray, len(list))

	for i, ix := range list {
		v, err := r.resolve(e.Value, innerT, fork.with(Part{Root: e.Call, Index: ix}))
		if err != nil {
			return nil, fmt.Errorf("merge of %s index %d: %w", e.Call.Fqid, i, err)
		}

		out[i] = v
	}

	return out, nil
}

// mergeElemType is the type of one merged value (core/resolve.go:295-323).
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
// (core/fork.go:1392-1790); otherwise the indices are those of the staged
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

	s, err := r.sourceExp(root, outer)
	if err != nil {
		return nil, err
	}

	c := counter{r: r, sp: sp, outer: outer}

	idx, err := c.source(s.Source)
	if _, null := s.Value.(*syntax.NullExp); null {
		idx, err = nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("indices of %s: %w", root.Fqid, err)
	}

	return idx, nil
}

// counter counts the forks of split sp at outer from its specialized
// source case for case as mrp expands them (countWalk): a literal by its
// length or keys, whatever its elements read, a disable-wrapped value by
// its value where its gate holds it enabled and by none where the gate
// disables it, and any other value by the collection it resolves to
// (core/fork.go:1657 expandForkFromObj). At the post of a stage the gate
// disables too, the gate is unstaged and holds the value enabled.
type counter struct {
	r     *resolver
	sp    *syntax.SplitExp
	outer Fork
}

func (c counter) source(src syntax.MapCallSource) ([]syntax.CollectionIndex, error) {
	switch s := src.(type) {
	case *syntax.MergeExp:
		if s.ForkNode != nil {
			return c.value(s.ForkNode)
		}

		return c.source(s.MergeOver)
	case *syntax.MapCallSet:
		return c.source(s.Master)
	case syntax.Exp:
		return c.exp(s)
	case *syntax.BoundReference:
		return c.value(s.Exp)
	default:
		return nil, fmt.Errorf("%w: fork source %T", errShape, src)
	}
}

func (c counter) exp(e syntax.Exp) ([]syntax.CollectionIndex, error) {
	switch x := e.(type) {
	case nil, *syntax.NullExp:
		return nil, nil
	case *syntax.MergeExp:
		return c.source(x)
	case *syntax.ArrayExp:
		return arrayIndices(len(x.Value)), nil
	case *syntax.MapExp:
		return mapIndices(slices.Collect(maps.Keys(x.Value))), nil
	case *syntax.DisabledExp:
		if d, ok := x.Disabled.(*syntax.SplitExp); ok && d.Call == c.sp.Call {
			return c.exp(d.Value)
		}

		if on, err := c.enabled(x); err != nil || !on {
			return nil, err
		}

		return c.exp(x.Value)
	case *syntax.SplitExp:
		return c.split(x)
	default:
		return c.value(e)
	}
}

// enabled reports whether d's gate holds its value enabled at the outer
// fork, as it does, unstaged, at the post of a stage it disables too.
func (c counter) enabled(d *syntax.DisabledExp) (bool, error) {
	off, err := c.r.disabledBy(d.Disabled, c.outer)

	switch {
	case errors.Is(err, ErrNotStaged) && c.r.g.allGatedBy((&countWalk{g: c.r.g, sp: c.sp}).exp(d.Value), d.Disabled):
		return true, nil
	case err != nil:
		return false, err
	default:
		return !off, nil
	}
}

// split counts a split over the forks of x's call, its own or the outer
// fork's element of another call's (core/fork.go:1491
// expandForkPartFromSplit).
func (c counter) split(x *syntax.SplitExp) ([]syntax.CollectionIndex, error) {
	if x.Call == c.sp.Call {
		return c.exp(x.Value)
	}

	p, ok := c.outer.part(x.Call)
	if !ok {
		return nil, fmt.Errorf("%w: split over %s outside its fork", errShape, x.GoString())
	}

	el, err := c.inner(x.Value, p.Index)
	if err != nil {
		return nil, err
	}

	if e, ok := el.(syntax.Exp); ok {
		return c.exp(e)
	}

	col, err := indexedOf(el, c.sp.Source.CallMode())

	return col.indices(), err
}

// literalElement is the expression at ix of an array or map literal e.
func literalElement(e syntax.Exp, ix syntax.CollectionIndex) (syntax.Exp, error) {
	switch x := e.(type) {
	case *syntax.ArrayExp:
		if i := ix.ArrayIndex(); i >= 0 && i < len(x.Value) {
			return x.Value[i], nil
		}

		return nil, fmt.Errorf("%w: array index %d >= %d", errNoElement, ix.ArrayIndex(), len(x.Value))
	case *syntax.MapExp:
		if v, ok := x.Value[ix.MapKey()]; ok {
			return v, nil
		}

		return nil, fmt.Errorf("%w: map key %q not found", errNoElement, ix.MapKey())
	default:
		return nil, fmt.Errorf("%w: %T is no literal collection", errShape, e)
	}
}

// inner is the element at ix of the collection e an outer split splits
// (core/fork.go:1532 expandForkSplitInnerPart): an element of a literal, as
// the expression it is, nothing where a gate disables e, and the element of
// a reference's or nested split's value.
func (c counter) inner(e syntax.Exp, ix syntax.CollectionIndex) (json.Marshaler, error) {
	switch x := e.(type) {
	case *syntax.ArrayExp, *syntax.MapExp:
		return literalElement(x, ix)
	case *syntax.DisabledExp:
		if on, err := c.enabled(x); err != nil || !on {
			return nil, err
		}

		return c.inner(x.Value, ix)
	case *syntax.SplitExp:
		p, ok := c.outer.part(x.Call)
		if !ok {
			return nil, fmt.Errorf("%w: split over %s outside its fork", errShape, x.GoString())
		}

		v, err := c.inner(x.Value, p.Index)
		if err != nil || v == nil {
			return nil, err
		}

		if ve, ok := v.(syntax.Exp); ok {
			return c.inner(ve, ix)
		}

		return element(v, ix)
	case *syntax.RefExp:
		v, err := c.r.resolveRef(x, nil, c.outer)
		if err != nil {
			return nil, err
		}

		return element(v, ix)
	default:
		return nil, fmt.Errorf("%w: split source %T", errShape, e)
	}
}

// value counts the collection e resolves to; a reference is read with its
// declared type, as core expandForkFromRef reads it (core/fork.go:1631).
func (c counter) value(e syntax.Exp) ([]syntax.CollectionIndex, error) {
	var (
		v   json.Marshaler
		err error
	)

	if ref, ok := e.(*syntax.RefExp); ok {
		v, err = c.r.resolveRef(ref, nil, c.outer)
	} else {
		v, err = c.r.resolve(e, c.sp.Type, c.outer)
	}

	if err != nil {
		return nil, err
	}

	col, err := indexedOf(v, c.sp.Source.CallMode())

	return col.indices(), err
}

// ownSource is the producer's output root's split source reads under
// outer, as its post reads it: the specialized source, unwrapped of the
// gates that disable the producer too, when that is a reference to the
// producer's outs; false for any other source, whose elements no one
// stage's outs hold.
func (r *resolver) ownSource(root *syntax.CallGraphStage, outer Fork) (*syntax.RefExp, bool, error) {
	prod, fed := r.g.Producer(root)
	if !fed {
		return nil, false, nil
	}

	s, err := r.sourceExp(root, outer)
	if err != nil {
		return nil, false, err
	}

	v := s.Value
	for d, ok := v.(*syntax.DisabledExp); ok && r.g.allGatedBy([]string{prod}, d.Disabled); d, ok = v.(*syntax.DisabledExp) {
		v = d.Value
	}

	ref, ok := v.(*syntax.RefExp)

	return ref, ok && ref.Id == prod, nil
}

// sourceExp specializes root's split for outer, as core Fork.expandForkPart
// does before it expands a fork from the split's source (core/fork.go:1392).
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

// knownIndices are the indices of a source of compile-time length: array
// positions, or map keys in byte order (core/resolve.go:525-564).
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
