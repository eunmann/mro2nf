package cg

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"

	"github.com/eunmann/mro2nf/internal/mrcore"
	"github.com/martian-lang/martian/martian/syntax"
)

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
		if off, err := r.disables(gate, fork); err != nil || off {
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

// sourceIndices are root's fork indices under outer as Martian core
// expands them, from what the task staged; ErrNotStaged where that does not
// hold the source the count reads.
func (r *resolver) sourceIndices(root *syntax.CallGraphStage, outer Fork) ([]syntax.CollectionIndex, error) {
	idx, err := r.core().Indices(root, r.g.outerRoots(root), coreFork(outer))
	if errors.Is(err, mrcore.ErrUnresolved) {
		return nil, fmt.Errorf("indices of %s: %w: %w", root.Fqid, ErrNotStaged, err)
	}

	if err != nil {
		return nil, fmt.Errorf("indices of %s: %w", root.Fqid, err)
	}

	for i, ix := range idx {
		idx[i] = concrete(ix)
	}

	return idx, nil
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
