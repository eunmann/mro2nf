// Package mrcore is Martian core's runtime resolution (core/resolve.go,
// core/fork.go at martian-lang/martian 50ad6c0) run over what one mro2nf task
// staged in place of a pipestance on disk. See LICENSE.martian.
//
// Only the data access differs from core: a fork's outs come from a Store,
// a fork's disabled state from its expansion alone, a node's forks from the
// fork asking for them (Node.view), and an outs read the task did not stage
// fails loudly rather than reading as not yet ready.
package mrcore

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/martian-lang/martian/martian/core"
	"github.com/martian-lang/martian/martian/syntax"
)

type (
	// LazyArgumentMap is core's: a stage's outs, each output's raw JSON.
	LazyArgumentMap = core.LazyArgumentMap
	// MarshalerMap is core's: a resolved map or struct.
	MarshalerMap = core.MarshalerMap
)

var (
	// ErrNotStaged reports an outs read the task did not stage.
	ErrNotStaged = errors.New("upstream outs not staged")

	// ErrNoElement is core's element error for an array index past the end
	// or a map key the map lacks (getArrayElement, getMapElement): mrp runs
	// a job whose binding reads one, the element null.
	ErrNoElement = errors.New("no such element")

	errNotReady    = errors.New("binding not ready")
	errUnknownNode = errors.New("no call graph node")
)

// Store serves the outs of the upstream stage forks one task staged.
type Store interface {
	// Outs is stage st's outs at fork, one part per fork root of st, or an
	// error wrapping ErrNotStaged.
	Outs(st *syntax.CallGraphStage, fork ForkId) (LazyArgumentMap, error)
	// SplitElement is the element of split e at fork, where the task staged
	// it in place of the collection, of type t: false where it did not.
	SplitElement(e *syntax.SplitExp, t syntax.Type, fork ForkId, part *ForkSourcePart) (json.Marshaler, bool, error)
}

// NewTop is the call graph under root, its outs read from store.
func NewTop(root syntax.CallGraphNode, types *syntax.TypeLookup, store Store) *TopNode {
	top := &TopNode{types: types, store: store}
	closure := root.NodeClosure()
	top.allNodes = make(map[string]*Node, len(closure))

	for fqid, call := range closure {
		n := &Node{top: top, call: call}
		for _, r := range call.ForkRoots() {
			n.forkRoots = append(n.forkRoots, r.Call())
		}

		top.allNodes[fqid] = n
	}

	return top
}

// Types is the invocation's type table, as core's TopNode.Types.
func (t *TopNode) Types() *syntax.TypeLookup { return t.types }

// notReady is the error of a binding that read as not ready: the reads the
// task did not stage.
func (t *TopNode) notReady() error {
	return fmt.Errorf("%w: %w", errNotReady, errors.Join(t.unstaged...))
}

func (t *TopNode) node(fqid string) (*Node, error) {
	n, ok := t.allNodes[fqid]
	if !ok {
		return nil, fmt.Errorf("%w: %s", errUnknownNode, fqid)
	}

	return n, nil
}

// ensureForks builds the node's forks on first use (core Node.buildForks,
// node.go:432).
func (n *Node) ensureForks() {
	if n.built {
		return
	}

	n.built = true
	n.forkIds.MakeForkIds(n.call.ForkRoots(), n.top.types)
	n.setForks()
}

// setForks makes the node's forks of its fork id list.
func (n *Node) setForks() {
	if len(n.forkIds.List) == 0 {
		n.forks = []*Fork{newFork(n, nil)}

		return
	}

	n.forks = make([]*Fork, len(n.forkIds.List), cap(n.forkIds.List))
	for i, id := range n.forkIds.List {
		n.forks[i] = newFork(n, id)
	}
}

// key names f by each part's call and index. GoString prints each part's
// split, whose source can be the whole collection of an outer fork.
func (f ForkId) key() string {
	var b strings.Builder

	for _, p := range f {
		fmt.Fprintf(&b, "%s@%p=", p.Split.Call.Id, p.Split.Call)

		switch {
		case p.Id == nil || p.Id.IndexSource() != nil:
			b.WriteByte('?')
		case p.Id.Mode() == syntax.ModeMapCall:
			b.WriteString(strconv.Quote(p.Id.MapKey()))
		case p.Id.Mode() == syntax.ModeArrayCall:
			b.WriteString(strconv.Itoa(p.Id.ArrayIndex()))
		default:
			b.WriteString("empty")
		}

		b.WriteByte(';')
	}

	return b.String()
}

// determined reports whether every part of f has a concrete index. Such an
// id names the one fork of its node that matches it, which core finds among
// the node's expanded forks (Node.matchFork) and a task builds alone: its
// outs read fails loudly where the task did not stage that fork.
func (f ForkId) determined() bool {
	for _, p := range f {
		if p.Id == nil || p.Id.IndexSource() != nil {
			return false
		}
	}

	return true
}

func newFork(node *Node, id ForkId) *Fork {
	f := &Fork{node: node}
	f.updateId(id)

	return f
}

// view is the node as fork scope sees it: core's forks of the node that
// match scope (ForkId.Matches), expanded by core's own Fork.expand. Core
// expands every fork of the node from the whole run's outs; a task has only
// those of its own forks, so the forks start from scope's own indices of
// the roots it shares with the node, and only the rest expand.
func (n *Node) view(scope ForkId) *Node {
	key := scope.key()
	if v, ok := n.views[key]; ok {
		return v
	}

	v := &Node{top: n.top, call: n.call, forkRoots: n.forkRoots, built: true}
	v.forkIds.makeScopedForkIDs(n.call.ForkRoots(), scope, n.top.types)
	v.setForks()
	v.expandForks(true)

	if n.views == nil {
		n.views = map[string]*Node{}
	}

	n.views[key] = v

	return v
}

// makeScopedForkIDs is MakeForkIds (fork.go:225) with each root scope gives
// an index taking that index alone, for which MakeForkIds would list every
// part of a root of known length.
func (set *ForkIdSet) makeScopedForkIDs(srcs syntax.ForkRootList, scope ForkId, lookup *syntax.TypeLookup) {
	if len(srcs) == 0 {
		return
	}

	parts := make([][]*ForkSourcePart, len(srcs))
	count := 1
	index := scope.SourceIndexMap()

	for i, src := range srcs {
		if p, err := scope.matchPart(src.Call()); err == nil && p.Id.IndexSource() == nil {
			parts[i] = []*ForkSourcePart{{Split: src.Split(), Id: p.Id, Range: p.Range}}
		} else {
			parts[i] = scopedParts(src.Split(), index, lookup)
		}

		count *= len(parts[i])
	}

	if len(srcs) == 1 {
		set.List = make([]ForkId, len(parts[0]))
		for i, p := range parts[0] {
			set.List[i] = ForkId{p}
		}

		return
	}

	set.List = make([]ForkId, count)
	for i := range set.List {
		set.List[i] = make(ForkId, 0, len(srcs))
	}

	stride := 1

	for _, these := range parts {
		for i := 0; i < len(set.List); {
			for _, p := range these {
				for range stride {
					set.List[i] = append(set.List[i], p)
					i++
				}
			}
		}

		stride *= len(these)
	}

	set.expandStaticForks(lookup)
}

// scopedParts are the parts makeForkIdParts (fork.go:183) lists for split,
// specialized first for the scope's indices as expandStaticForks would
// specialize it (getForkSrc): a root inside an outer fork lists its own
// element's parts, where the unspecialized source's length and keys walk
// every outer element. A source of unknown length is one undetermined part,
// without the count of its forks makeForkIdParts adds, which only sizes the
// fork list and walks the whole source.
func scopedParts(split *syntax.SplitExp, index map[*syntax.CallStm]syntax.CollectionIndex,
	lookup *syntax.TypeLookup,
) []*ForkSourcePart {
	if len(index) > 0 {
		if exp, err := split.BindingPath("", index, lookup); err == nil {
			if s, ok := exp.(*syntax.SplitExp); ok && s.Call == split.Call {
				split = s
			}
		}
	}

	if !knownParts(split.Source) {
		return []*ForkSourcePart{{Split: split, Id: dummyForkId}}
	}

	parts, _ := makeForkIdParts(split, lookup)
	if len(parts) == 0 {
		return []*ForkSourcePart{{Split: split, Id: emptyFork{}, Range: mapSourceRange{Source: split.Source}}}
	}

	return parts
}

// knownParts reports whether makeForkIdParts lists src's parts outright.
func knownParts(src syntax.MapCallSource) bool {
	switch src.CallMode() {
	case syntax.ModeArrayCall:
		return src.ArrayLength() >= 0
	case syntax.ModeMapCall:
		return src.Keys() != nil
	case syntax.ModeNullMapCall:
		return true
	case syntax.ModeSingleCall, syntax.ModeUnknownMapCall:
		return false
	default:
		return false
	}
}

// tableParts is root src's part of core's ForkIdSet.Table (fork.go:225),
// the parts of the root's static source, which core builds once and never
// changes.
func (n *Node) tableParts(src *syntax.CallStm) []*ForkSourcePart {
	for _, r := range n.call.ForkRoots() {
		if r.Call() == src {
			parts, _ := makeForkIdParts(r.Split(), n.top.types)

			return parts
		}
	}

	return nil
}

// fromCore is v, a value core's exported LazyArgumentMap.Path returned, with
// each array in it as this package's marshallerArray: Path projects
// through an array into core's own, unexported one, which the copied
// element and length reads (getArrayElement, getUnknownLength) take only
// as their own package's.
func fromCore(v json.Marshaler) json.Marshaler {
	switch x := v.(type) {
	case nil, json.RawMessage, marshallerArray:
		return v
	case MarshalerMap:
		for k, e := range x {
			x[k] = fromCore(e)
		}

		return x
	}

	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Slice || rv.Type().Elem() != reflect.TypeFor[json.Marshaler]() {
		return v
	}

	if rv.IsNil() {
		return marshallerArray(nil)
	}

	out := make(marshallerArray, rv.Len())
	for i := range out {
		if e, ok := reflect.TypeAssert[json.Marshaler](rv.Index(i)); ok {
			out[i] = fromCore(e)
		}
	}

	return out
}

// outs is the fork's outs: all null where its expansion disabled it, as
// core writeDisable writes them (stage.go:929, makeOutArgs).
func (f *Fork) outs() (LazyArgumentMap, error) {
	st, ok := f.node.call.(*syntax.CallGraphStage)
	if !ok {
		return nil, fmt.Errorf("%w: %s is no stage", ErrNotStaged, f.fqname)
	}

	if f.disabled {
		out := LazyArgumentMap{}
		if ps := st.Callable().GetOutParams(); ps != nil {
			for _, p := range ps.List {
				out[p.GetId()] = json.RawMessage("null")
			}
		}

		return out, nil
	}

	outs, err := f.node.top.store.Outs(st, f.forkId)
	if err != nil {
		return nil, fmt.Errorf("outs of %s: %w", f.fqname, err)
	}

	return outs, nil
}
