package cg

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/martian-lang/martian/martian/syntax"
)

var errBadFork = errors.New("invalid fork")

// ArrayIndex is a concrete fork index into an array-mapped call.
type ArrayIndex int

// GoString implements syntax.CollectionIndex.
func (i ArrayIndex) GoString() string { return strconv.Itoa(int(i)) }

// Mode implements syntax.CollectionIndex.
func (ArrayIndex) Mode() syntax.CallMode { return syntax.ModeArrayCall }

// MapKey implements syntax.CollectionIndex; an array index has none.
func (ArrayIndex) MapKey() string { return "" }

// ArrayIndex implements syntax.CollectionIndex.
func (i ArrayIndex) ArrayIndex() int { return int(i) }

// IndexSource implements syntax.CollectionIndex: the index is concrete.
func (ArrayIndex) IndexSource() syntax.MapCallSource { return nil }

// MapKey is a concrete fork key into a map-mapped call.
type MapKey string

// GoString implements syntax.CollectionIndex.
func (k MapKey) GoString() string { return strconv.Quote(string(k)) }

// Mode implements syntax.CollectionIndex.
func (MapKey) Mode() syntax.CallMode { return syntax.ModeMapCall }

// MapKey implements syntax.CollectionIndex.
func (k MapKey) MapKey() string { return string(k) }

// ArrayIndex implements syntax.CollectionIndex; a map key has none.
func (MapKey) ArrayIndex() int { return -1 }

// IndexSource implements syntax.CollectionIndex: the key is concrete.
func (MapKey) IndexSource() syntax.MapCallSource { return nil }

// Part is one dimension of a fork: a map call (its root) and the index.
type Part struct {
	Root  *syntax.CallGraphStage
	Index syntax.CollectionIndex
}

// Fork is a stage instance's position: one part per fork root, in the order
// of the node's ForkRoots, as core orders a ForkId.
type Fork []Part

// String renders f as its JSON encoding, which no two forks share.
func (f Fork) String() string {
	raw, err := json.Marshal(f.encode())
	if err != nil {
		return fmt.Sprintf("%d-part fork", len(f))
	}

	return string(raw)
}

// project restricts f to roots, taking each root's index from f, else from
// the reference's own fork indices, as core ForkId.Match does
// (core/fork.go:453).
func (f Fork) project(roots syntax.ForkRootList, ref map[*syntax.CallStm]syntax.CollectionIndex) (Fork, error) {
	out := make(Fork, 0, len(roots))

	for _, r := range roots {
		if p, ok := f.part(r.Call()); ok {
			out = append(out, Part{Root: r, Index: p.Index})

			continue
		}

		ix, ok := ref[r.Call()]
		if !ok || ix == nil || ix.IndexSource() != nil {
			return nil, fmt.Errorf("%w: no index for %s in %s", errBadFork, r.Fqid, f)
		}

		out = append(out, Part{Root: r, Index: concrete(ix)})
	}

	return out, nil
}

// restrict is f's parts for roots, and false when f lacks one.
func (f Fork) restrict(roots syntax.ForkRootList) (Fork, bool) {
	out := make(Fork, 0, len(roots))

	for _, r := range roots {
		p, ok := f.part(r.Call())
		if !ok {
			return nil, false
		}

		out = append(out, Part{Root: r, Index: p.Index})
	}

	return out, true
}

// indexMap is f as BindingPath takes it (core ForkId.SourceIndexMap).
func (f Fork) indexMap() map[*syntax.CallStm]syntax.CollectionIndex {
	if len(f) == 0 {
		return nil
	}

	m := make(map[*syntax.CallStm]syntax.CollectionIndex, len(f))
	for _, p := range f {
		m[p.Root.Call()] = p.Index
	}

	return m
}

func (f Fork) part(call *syntax.CallStm) (Part, bool) {
	for _, p := range f {
		if p.Root.Call() == call {
			return p, true
		}
	}

	return Part{}, false
}

// rootPart is f's part for root itself. Staged items of different nodes are
// matched by root fqid: two instantiations of one sub-pipeline share their
// map calls' CallStm, which part matches by, as core matches fork parts
// within one node's resolution.
func (f Fork) rootPart(root *syntax.CallGraphStage) (Part, bool) {
	i, ok := f.rootPos(root)
	if !ok {
		return Part{}, false
	}

	return f[i], true
}

// rootPos is the position of root's part in f, matched as rootPart matches.
func (f Fork) rootPos(root *syntax.CallGraphStage) (int, bool) {
	for i, p := range f {
		if p.Root.Fqid == root.Fqid {
			return i, true
		}
	}

	return 0, false
}

// with returns a copy of f with p appended.
func (f Fork) with(p Part) Fork {
	out := make(Fork, len(f), len(f)+1)
	copy(out, f)

	return append(out, p)
}

func (f Fork) key() string { return f.String() }

func concrete(ix syntax.CollectionIndex) syntax.CollectionIndex {
	if ix.Mode() == syntax.ModeMapCall {
		return MapKey(ix.MapKey())
	}

	return ArrayIndex(ix.ArrayIndex())
}

func indexEqual(a, b syntax.CollectionIndex) bool {
	if a.Mode() != b.Mode() {
		return false
	}

	if a.Mode() == syntax.ModeMapCall {
		return a.MapKey() == b.MapKey()
	}

	return a.ArrayIndex() == b.ArrayIndex()
}

// partJSON is a fork part on disk. A map key travels as a JSON string, so a
// key holding any character (tab, newline, comma, quote) round-trips exactly.
type partJSON struct {
	Root  string  `json:"root"`
	Index *int    `json:"index,omitempty"`
	Key   *string `json:"key,omitempty"`
}

func (f Fork) encode() []partJSON {
	out := make([]partJSON, len(f))

	for i, p := range f {
		out[i].Root = p.Root.Fqid

		if p.Index.Mode() == syntax.ModeMapCall {
			k := p.Index.MapKey()
			out[i].Key = &k
		} else {
			n := p.Index.ArrayIndex()
			out[i].Index = &n
		}
	}

	return out
}

func (g *Graph) decodeFork(parts []partJSON) (Fork, error) {
	f := make(Fork, len(parts))

	for i, p := range parts {
		r, err := g.forkRoot(p.Root)
		if err != nil {
			return nil, err
		}

		switch {
		case p.Key != nil && p.Index == nil:
			f[i] = Part{Root: r, Index: MapKey(*p.Key)}
		case p.Index != nil && p.Key == nil:
			f[i] = Part{Root: r, Index: ArrayIndex(*p.Index)}
		default:
			return nil, fmt.Errorf("%w: part %d of root %s needs exactly one of index and key", errBadFork, i, p.Root)
		}
	}

	return f, nil
}
