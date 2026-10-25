package mrcore

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/martian-lang/martian/martian/syntax"
)

// ErrUnresolved reports a fork root whose forks the task could not count.
var ErrUnresolved = errors.New("fork root unresolved")

// Part is the fork id part of root at index ix.
func Part(root *syntax.CallGraphStage, ix syntax.CollectionIndex) *ForkSourcePart {
	return &ForkSourcePart{Split: root.Split(), Id: convertForkPart(ix)}
}

// Index is the index of root's part in fork, and false where fork has none.
func (f ForkId) Index(root *syntax.CallGraphStage) (syntax.CollectionIndex, bool) {
	p, err := f.matchPart(root.Call())
	if err != nil || p.Id.IndexSource() != nil {
		return nil, false
	}

	return p.Id, true
}

// rootsNode is a call graph node forked on roots: the forks core expands of
// any node forked on a root's outer roots and the root itself.
type rootsNode struct {
	syntax.CallGraphNode

	roots syntax.ForkRootList
}

func (n rootsNode) ForkRoots() syntax.ForkRootList { return n.roots }

// Indices lists the indices of root's forks under outer, as core expands
// root's part (Fork.expand) in any node forked on outerRoots and root: the
// node's forks that match outer. A part the task cannot expand, its source
// unstaged, fails with ErrUnresolved.
func (t *TopNode) Indices(root *syntax.CallGraphStage, outerRoots syntax.ForkRootList, outer ForkId) ([]syntax.CollectionIndex, error) {
	t.unstaged = nil

	roots := slices.Concat(outerRoots, syntax.ForkRootList{root})
	node := &Node{top: t, call: rootsNode{CallGraphNode: root, roots: roots}}

	for _, r := range roots {
		node.forkRoots = append(node.forkRoots, r.Call())
	}

	v := node.view(outer)
	if v.expandErr != nil {
		return nil, v.expandErr
	}

	var out []syntax.CollectionIndex

	for _, f := range v.forks {
		if len(f.forkId) == 0 {
			continue
		}

		p, err := f.forkId.matchPart(root.Call())
		if err != nil {
			return nil, fmt.Errorf("forks of %s: %w", root.Fqid, err)
		}

		switch {
		case p.Id.IndexSource() != nil && t.splitRemoved(root, f.forkId):
		case p.Id.IndexSource() != nil:
			return nil, fmt.Errorf("%w: %s at %s: %w", ErrUnresolved, root.Fqid, outer.GoString(), errors.Join(t.unstaged...))
		case p.Id.Mode() == syntax.ModeNullMapCall:
		default:
			out = append(out, p.Id)
		}
	}

	return out, nil
}

// splitRemoved reports whether root's split, specialized for fork, is no
// split, which core's expandForkPart leaves unexpanded (fork.go:1392): a
// split of a value a gate disables, or of null, forks nothing.
func (t *TopNode) splitRemoved(root *syntax.CallGraphStage, fork ForkId) bool {
	exp, err := root.Split().BindingPath("", fork.SourceIndexMap(), t.types)
	_, split := exp.(*syntax.SplitExp)

	return err == nil && !split
}

// Elements is the elements of v where it is an array a resolution returned.
func Elements(v json.Marshaler) ([]json.Marshaler, bool) {
	a, ok := v.(marshallerArray)

	return a, ok
}

// Inputs resolves the input bindings of stage fqid at fork as the job's
// _args resolve them (core Node.resolveInputs). An input that reads only
// elements its collection lacks keeps the value resolved around them, as
// mrp runs the job with them null, and that error is returned second; any
// other error fails, returned third.
func (t *TopNode) Inputs(fqid string, fork ForkId) (MarshalerMap, error, error) {
	node, err := t.node(fqid)
	if err != nil {
		return nil, nil, err
	}

	t.unstaged = nil

	args, err := node.resolveInputs(fork)

	switch {
	case err == nil:
		return args, nil, nil
	case onlyMissing(err):
		return args, err, nil
	default:
		return nil, nil, fmt.Errorf("resolve inputs of %s at %s: %w", fqid, fork.GoString(), t.withUnstaged(err))
	}
}

// withUnstaged is err with the reads the task did not stage, which left
// what err reports unresolved.
func (t *TopNode) withUnstaged(err error) error {
	if len(t.unstaged) == 0 || errors.Is(err, errNotReady) {
		return err
	}

	return fmt.Errorf("%w (%w)", err, t.notReady())
}

// Resolve evaluates exp of type typ at fork (core TopNode.resolve).
func (t *TopNode) Resolve(exp syntax.Exp, typ syntax.Type, fork ForkId) (json.Marshaler, error) {
	t.unstaged = nil

	ready, v, err := t.resolve(exp, typ, fork, 0)

	switch {
	case err != nil:
		return v, t.withUnstaged(err)
	case !ready:
		return nil, t.notReady()
	default:
		return v, nil
	}
}

// onlyMissing reports whether every error err holds is a missing element:
// each of a joined or listed error's, and along a single chain its leaf.
func onlyMissing(err error) bool {
	var multi interface{ Unwrap() []error }
	if errors.As(err, &multi) {
		return allMissing(multi.Unwrap())
	}

	if list, ok := errors.AsType[syntax.ErrorList](err); ok {
		return len(list) > 0 && allMissing(list)
	}

	return errors.Is(err, ErrNoElement)
}

func allMissing(errs []error) bool {
	for _, e := range errs {
		if !onlyMissing(e) {
			return false
		}
	}

	return true
}
