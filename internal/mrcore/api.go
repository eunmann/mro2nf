package mrcore

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/martian-lang/martian/martian/syntax"
)

var (
	// ErrUnresolved reports a fork root whose forks the task could not count.
	ErrUnresolved = errors.New("fork root unresolved")

	// ErrInvalidOuts reports outs mrp's output checks reject.
	ErrInvalidOuts = errors.New("invalid outs")

	errOtherKey = errors.New("not the fork's own key")
)

// Part is the fork id part of root at index ix.
func Part(root *syntax.CallGraphStage, ix syntax.CollectionIndex) *ForkSourcePart {
	return &ForkSourcePart{Split: root.Split(), Id: convertForkPart(ix)}
}

// CountedPart is root's part at index ix among the n forks of root under
// the parts before it, as core's expansion leaves it (fork.go:1657
// expandForkFromObj, :350 expandStaticForkPart): root's split, and a range
// of length n, which ForkId.ForkIdString reads where root's source is of
// unknown length.
func CountedPart(root *syntax.CallGraphStage, ix syntax.CollectionIndex, n int) *ForkSourcePart {
	p := Part(root, ix)
	if p.Id.Mode() == syntax.ModeMapCall {
		p.Range = ownKeyRange{key: p.Id.MapKey(), n: n}
	} else {
		p.Range = arrayLengthRange(n)
	}

	return p
}

// ownKeyRange is the range of a map part of n keys of which a task knows
// only its own: core's mapKeyRange (expandForkFromObj) lists every key of
// the source, which the task did not stage.
type ownKeyRange struct {
	key string
	n   int
}

func (r ownKeyRange) Allow(i syntax.CollectionIndex) error {
	if i.MapKey() != r.key {
		return fmt.Errorf("%w: %q, the fork's is %q", errOtherKey, i.MapKey(), r.key)
	}

	return nil
}

func (r ownKeyRange) Length() int { return r.n }

// ForkID is the id mrp names the fork of node fqid at fork by, its fork
// dir's and its jobs' (ForkId.ForkIdString): the id of the node's fork that
// fork matches (Node.matchFork), whose parts carry their ranges
// (CountedPart).
func (t *TopNode) ForkID(fqid string, fork ForkId) (string, error) {
	node, err := t.node(fqid)
	if err != nil {
		return "", err
	}

	f, err := node.matchFork(nil, fork)
	if err != nil {
		return "", fmt.Errorf("fork of %s at %s: %w", fqid, fork.GoString(), err)
	}

	id, err := f.forkId.ForkIdString()
	if err != nil {
		return "", fmt.Errorf("fork id of %s at %s: %w", fqid, fork.GoString(), err)
	}

	return id, nil
}

// Disabled reports whether the fork of node fqid at fork is disabled, as
// core's Fork.disabled decides it (stage.go:883): a part of no forks, or a
// disable binding that resolves true. A binding that fails to resolve to a
// bool, or reads what the task did not stage, fails.
func (t *TopNode) Disabled(fqid string, fork ForkId) (bool, error) {
	node, err := t.node(fqid)
	if err != nil {
		return false, err
	}

	t.unstaged = nil

	off, err := newFork(node, fork).disabled()
	if err != nil {
		return false, fmt.Errorf("disabled state of %s at %s: %w", fqid, fork.GoString(), t.withUnstaged(err))
	}

	return off, nil
}

// Disables reports whether gate, the gate of a disable-wrapped value or a
// disable binding, disables at fork, as core's resolveDisabledExp decides it
// (resolve.go:248).
func (t *TopNode) Disables(gate syntax.Exp, fork ForkId) (bool, error) {
	t.unstaged = nil

	ready, off, err := t.gate(gate, fork, 0)

	switch {
	case err != nil:
		return false, t.withUnstaged(err)
	case !ready:
		return false, t.notReady()
	default:
		return off, nil
	}
}

// VerifyOutput checks outs, the outs of stage fqid's fork at fork, as mrp
// checks a finished stage's (stage.go:764 Fork.verifyOutput): an error
// fails it, wrapping ErrInvalidOuts.
func (t *TopNode) VerifyOutput(fqid string, fork ForkId, outs LazyArgumentMap) error {
	node, err := t.node(fqid)
	if err != nil {
		return err
	}

	if ok, msg := newFork(node, fork).verifyOutput(outs); !ok {
		return fmt.Errorf("%w of %s: %s", ErrInvalidOuts, fqid, msg)
	}

	return nil
}

// PipelineOutputs resolves the outs of pipeline fqid's fork at fork and
// checks them, as mrp's Fork.stepPipeline does (stage.go:1358-1366,
// resolvePipelineOutputs then verifyPipelineOutput): the outs and their
// type, or an error, which wraps ErrInvalidOuts where the check rejects
// them.
func (t *TopNode) PipelineOutputs(fqid string, fork ForkId) (json.Marshaler, syntax.Type, error) {
	node, err := t.node(fqid)
	if err != nil {
		return nil, nil, err
	}

	t.unstaged = nil

	outs, typ, err := node.resolvePipelineOutputs(fork)
	if err != nil {
		return nil, nil, fmt.Errorf("outs of %s: %w", fqid, t.withUnstaged(err))
	}

	if f := newFork(node, fork); len(f.OutParams().List) > 0 {
		if ok, msg := f.verifyPipelineOutput(outs, typ); !ok {
			return nil, nil, fmt.Errorf("%w of %s: %s", ErrInvalidOuts, fqid, msg)
		}
	}

	return outs, typ, nil
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
